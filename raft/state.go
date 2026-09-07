package raft

import (
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"
)

type NodeState int

const (
	Follower NodeState = iota
	Candidate
	Leader
)

const (
	// electionTimeoutBaseMs is the minimum election timeout in milliseconds.
	electionTimeoutBaseMs = 150
	// electionTimeoutJitterMs bounds the additional randomized election
	// timeout (added on top of the base) to prevent split votes.
	electionTimeoutJitterMs = 150
	// heartbeatInterval is the leader heartbeat period.
	heartbeatInterval = 50 * time.Millisecond
	// raftTickInterval is how often the node event loop ticks.
	raftTickInterval = 50 * time.Millisecond
	// quorumDivisor is used to compute a majority quorum (n/quorumDivisor + 1).
	quorumDivisor = 2
	// requestVoteTimeout bounds a single RequestVote RPC.
	requestVoteTimeout = 100 * time.Millisecond
	// appendEntriesTimeout bounds a single AppendEntries RPC.
	appendEntriesTimeout = 50 * time.Millisecond
	// installSnapshotTimeout bounds a single InstallSnapshot RPC — that is, one
	// chunk, not a whole snapshot transfer.
	installSnapshotTimeout = 5 * time.Second
	// minElectionTimeout is the shortest election timeout any node uses. A
	// follower that heard from its leader more recently than this cannot yet have
	// timed out, so a RequestVote arriving inside that window comes from a server
	// that is not seeing the same leader — see RequestVote's disruption check
	// (paper §6, "Servers disregard RequestVote RPCs when they believe a current
	// leader exists").
	minElectionTimeout = electionTimeoutBaseMs * time.Millisecond
	// defaultSnapshotChunkSize is how many payload bytes one InstallSnapshot RPC
	// carries. It bounds the size and the duration of a single RPC, which is what
	// decides how long that peer's replication slot is occupied by one message
	// (KNOWN_ISSUES.md R15); it does not bound how much either side holds in
	// memory, since the leader reads the whole envelope and the receiver buffers
	// the whole payload before installing it.
	defaultSnapshotChunkSize = 64 * 1024
)

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Unknown"
	}
}

type PersistentState struct {
	CurrentTerm int
	VotedFor    *string
	Log         []LogEntry

	// Snapshot metadata
	LastIncludedIndex int // Index of last entry in snapshot
	LastIncludedTerm  int // Term of last entry in snapshot

	// Config is the cluster configuration currently in effect: the last
	// configuration entry in Log, or SnapshotConfig when Log holds none. Paper
	// §6 puts a configuration into effect the moment its entry reaches the log,
	// committed or not, so this is derived state that must be written in the same
	// persist() as the log it was derived from — see recomputeConfigLocked.
	//
	// It is nil in a state file written before dynamic membership existed; the
	// peer list passed to NewRaftStateWithPersister then seeds it.
	Config *ClusterConfig

	// SnapshotConfig is the configuration in effect at LastIncludedIndex. It is
	// what Config falls back to when every configuration entry has been
	// compacted away or truncated off, so the paper's rule "revert to the
	// previous configuration when a configuration entry is removed from the log"
	// stays answerable after compaction.
	//
	// It lives here rather than in snapshot.json on purpose: it is Raft's own
	// state, it must be exactly the configuration the boundary it accompanies was
	// taken under, and raft_state.json is the file that already carries that
	// boundary atomically. Keeping the two in one file means log compaction never
	// has to rewrite the state machine's snapshot, and leaves both R3's
	// payload-before-boundary ordering and VerifySnapshotConsistency untouched.
	SnapshotConfig *ClusterConfig
}

type VolatileState struct {
	CommitIndex int
	LastApplied int
}

type LeaderState struct {
	NextIndex  map[string]int
	MatchIndex map[string]int

	// inFlight marks the peers that currently have a replication RPC outstanding
	// (an AppendEntries, or the InstallSnapshot replicateToPeer may fall through
	// to). Ticks skip a peer that is still busy, so at most one replication RPC
	// per peer is in flight at a time. Without it every 50ms tick spawned another
	// round regardless, letting a 5-second InstallSnapshot run alongside a stream
	// of AppendEntries to the same follower and letting their replies land out of
	// order. The map is recreated by initializeLeaderState, so a slot can never
	// leak across leader terms.
	inFlight map[string]bool
}

// Persister interface for saving/loading persistent state
type Persister interface {
	SaveRaftState(state *PersistentState) error
	LoadRaftState() (*PersistentState, error)
}

type RaftState struct {
	mu sync.RWMutex

	nodeID string
	state  NodeState
	// peers is the peer list this node was constructed with. It seeds the
	// initial cluster configuration and is kept only for that purpose plus
	// diagnostics: every decision about who votes, who is replicated to and what
	// constitutes a quorum comes from persistent.Config (raft/membership.go),
	// because the configuration changes at runtime and this list does not.
	peers []string

	persistent PersistentState
	volatile   VolatileState
	leader     *LeaderState

	currentLeader string // Track the current leader ID

	electionTimeout  time.Duration
	heartbeatTimeout time.Duration
	lastHeartbeat    time.Time
	electionTimer    *time.Timer

	applyCh   chan ApplyMsg
	persister Persister
	logger    *log.Logger

	// applyNotify wakes the applier goroutine when there is new work: the commit
	// index moved, or an installed snapshot is waiting to be handed over. It is
	// buffered with room for one token and written with a non-blocking send, so a
	// lock holder never blocks on it and repeated notifications coalesce into the
	// single "go look again" the applier needs.
	applyNotify chan struct{}
	// pendingSnapshot is the most recently installed snapshot that the applier
	// has not delivered yet, or nil. Written under rs.mu by the InstallSnapshot
	// receive path and taken by the applier under the same lock. Only the newest
	// one is kept: snapshots arrive in increasing index order (older ones are
	// rejected outright), so an undelivered one is always subsumed by its
	// successor.
	pendingSnapshot *ApplyMsg

	// pendingChunks is the snapshot currently being received chunk by chunk, or
	// nil when no transfer is in progress. Written and read under rs.mu by the
	// InstallSnapshot receive path only. It is deliberately invisible to every
	// other part of the node: until the final chunk arrives nothing about the
	// Raft state, the state machine or the disk reflects it, so an interrupted
	// transfer leaves no trace to clean up beyond dropping this field.
	pendingChunks *snapshotAssembly

	// Goroutine lifecycle. lifecycleMu guards the stopped flag so that spawn can
	// never call wg.Add after Stop has begun waiting; stopCh is the broadcast
	// every spawned goroutine (and the applier's channel sends) selects on.
	lifecycleMu sync.Mutex
	stopped     bool
	stopCh      chan struct{}
	wg          sync.WaitGroup

	// snapshotter is consulted by the leader when a follower needs InstallSnapshot.
	// May be nil if log compaction is not configured (snapshot RPCs will be skipped).
	snapshotter Snapshotter

	// snapshotChunkSize is how many payload bytes one InstallSnapshot RPC carries
	// on the send path. Guarded by rs.mu so a test can shrink it without racing
	// the replication goroutines that read it.
	snapshotChunkSize int
}

// getSnapshotChunkSize returns the size of one outgoing snapshot chunk.
func (rs *RaftState) getSnapshotChunkSize() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.snapshotChunkSize
}

// setSnapshotChunkSize changes the size of one outgoing snapshot chunk. It
// exists for tests, which need a snapshot to span several chunks without
// building a 64 KiB payload; production always runs at the default.
func (rs *RaftState) setSnapshotChunkSize(size int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.snapshotChunkSize = size
}

// snapshotAssembly is a snapshot being received in chunks: the generation it
// belongs to, who is sending it, and the bytes accumulated so far. It lives only
// in memory — nothing is written to snapshot.json until the transfer completes,
// so a transfer that is abandoned half way (the leader died, our term moved on,
// the node is shutting down) is discarded by dropping this value.
//
// leaderID and term are what the assembly is scoped to: a chunk from anyone else,
// or from an older term, does not belong to it. lastIncludedIndex and
// lastIncludedTerm pin the generation, so a leader that starts shipping a newer
// snapshot mid-transfer cannot have its bytes spliced onto the older one's
// (the same "one envelope, one generation" rule the send path follows,
// KNOWN_ISSUES.md R4).
type snapshotAssembly struct {
	leaderID          string
	term              int
	lastIncludedIndex int
	lastIncludedTerm  int
	buf               []byte
}

// matches reports whether args continues this assembly: same sender, same term,
// same snapshot generation. Callers must hold rs.mu.
func (a *snapshotAssembly) matches(args *InstallSnapshotArgs, currentTerm int) bool {
	return a.leaderID == args.LeaderID &&
		a.term == currentTerm &&
		a.lastIncludedIndex == args.LastIncludedIndex &&
		a.lastIncludedTerm == args.LastIncludedTerm
}

// discardSnapshotAssemblyLocked drops any partially received snapshot. Callers
// must hold rs.mu. Safe to call when there is none.
func (rs *RaftState) discardSnapshotAssemblyLocked(reason string) {
	if rs.pendingChunks == nil {
		return
	}
	rs.logger.Printf("InstallSnapshot: discarding %d buffered bytes of the snapshot at index %d from %s (%s)",
		len(rs.pendingChunks.buf), rs.pendingChunks.lastIncludedIndex, rs.pendingChunks.leaderID, reason)
	rs.pendingChunks = nil
}

// spawn starts fn in a goroutine that Stop will wait for, and reports whether it
// was started. It is the only way a RaftState creates a goroutine, so that
// shutdown has an exact list of what to join (KNOWN_ISSUES.md R19).
//
// After Stop has been entered nothing new is started and false is returned:
// callers that reserved something for the goroutine (the replication slot, a
// slot in a results channel) must undo that reservation themselves.
func (rs *RaftState) spawn(fn func()) bool {
	rs.lifecycleMu.Lock()
	if rs.stopped {
		rs.lifecycleMu.Unlock()
		return false
	}
	// Add under lifecycleMu, which Stop also takes before it starts waiting, so
	// an Add can never race a Wait that has already begun.
	rs.wg.Add(1)
	rs.lifecycleMu.Unlock()

	go func() {
		defer rs.wg.Done()
		fn()
	}()
	return true
}

// Stop closes the stop signal and blocks until every goroutine this RaftState
// started has returned — the applier, the replication and vote rounds, and the
// ReadIndex heartbeats. It is idempotent, and a concurrent second caller waits
// for the same set of goroutines rather than returning early.
//
// Returning from Stop is what makes it safe to close applyCh: the applier is the
// only sender on that channel and it has exited by then (KNOWN_ISSUES.md R19).
func (rs *RaftState) Stop() {
	rs.lifecycleMu.Lock()
	if !rs.stopped {
		rs.stopped = true
		close(rs.stopCh)
	}
	rs.lifecycleMu.Unlock()

	rs.wg.Wait()

	// Drop any half-received snapshot. Nothing outside this field reflects it,
	// so this is only about not holding the buffer past shutdown; a leader that
	// comes back to a restarted node starts the transfer again from offset 0.
	rs.mu.Lock()
	rs.discardSnapshotAssemblyLocked("node is stopping")
	rs.mu.Unlock()
}

// SetSnapshotter wires the state machine snapshotter so the leader can serve
// InstallSnapshot RPCs to lagging followers.
func (rs *RaftState) SetSnapshotter(s Snapshotter) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.snapshotter = s
}

type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int
	CommandTerm  int

	// ConfigChange marks a cluster configuration entry (KNOWN_ISSUES.md R14).
	// Such an entry is consensus bookkeeping, not a state machine command: its
	// Command is a JSON cluster configuration that the state machine cannot
	// decode. The state machine must advance its applied index over it — the
	// log-compaction accounting and the ReadIndex catch-up both need every
	// committed index accounted for — and otherwise skip it, exactly as it does
	// for the election no-op. It is delivered rather than filtered out in the
	// applier for that reason.
	ConfigChange bool

	// For snapshots
	SnapshotValid bool
	SnapshotIndex int
	SnapshotTerm  int
	SnapshotData  []byte

	// SnapshotPersisted reports that SnapshotData has already been written to
	// stable storage by the Raft layer, before the snapshot boundary was
	// persisted (see the ordering invariant on RaftState.InstallSnapshot). The
	// state machine must then only update its in-memory state and must not save
	// the payload a second time. It is false when no Snapshotter is wired (a
	// memory-only configuration), in which case persisting the payload is still
	// the state machine's job.
	SnapshotPersisted bool
}

func NewRaftState(nodeID string, peers []string, applyCh chan ApplyMsg) *RaftState {
	// A nil persister never touches disk, so construction cannot fail here.
	rs, _ := NewRaftStateWithPersister(nodeID, peers, applyCh, nil)
	return rs
}

func NewRaftStateWithPersister(nodeID string, peers []string, applyCh chan ApplyMsg, persister Persister) (*RaftState, error) {
	// Randomized election timeout between 150ms and 300ms
	// This prevents split votes when nodes start at the same time
	//nolint:gosec // G404: election timeout jitter does not need a crypto RNG
	randomTimeout := electionTimeoutBaseMs + rand.Intn(electionTimeoutJitterMs)

	rs := &RaftState{
		nodeID:            nodeID,
		state:             Follower,
		peers:             peers,
		persistent:        PersistentState{CurrentTerm: 0, VotedFor: nil, Log: make([]LogEntry, 0)},
		volatile:          VolatileState{CommitIndex: 0, LastApplied: 0},
		electionTimeout:   time.Duration(randomTimeout) * time.Millisecond,
		heartbeatTimeout:  heartbeatInterval,
		lastHeartbeat:     time.Now(),
		applyCh:           applyCh,
		applyNotify:       make(chan struct{}, 1),
		snapshotChunkSize: defaultSnapshotChunkSize,
		stopCh:            make(chan struct{}),
		persister:         persister,
		logger:            log.New(log.Writer(), "[RAFT-STATE-"+nodeID+"] ", log.LstdFlags),
	}

	// Load persistent state if a persister is configured. A missing state file
	// is not an error (LoadRaftState returns a zero-valued state), so this only
	// fails on a genuine read/corruption error. In that case we cannot trust
	// our on-disk term and vote, so we refuse to start rather than silently
	// resetting to term 0 and risking a double vote in a term we already acted in.
	if persister != nil {
		if err := rs.loadPersistentState(); err != nil {
			return nil, fmt.Errorf("refusing to start: cannot load persistent state: %w", err)
		}
	}

	// Seed the cluster configuration from the peer list when the state file
	// carried none — either because this is a fresh node or because the file
	// predates dynamic membership. A file that does carry one wins: it is the
	// configuration this node last agreed to, and the -peers flag may well be
	// stale by then (KNOWN_ISSUES.md R14).
	if rs.persistent.SnapshotConfig == nil {
		rs.persistent.SnapshotConfig = NewClusterConfig(peers)
	}
	// Re-derive the configuration in effect from the recovered log rather than
	// trusting the field. It costs one backwards scan at startup and makes
	// "Config is the last configuration entry in the durable log, else
	// SnapshotConfig" an invariant that holds from the first instruction — which
	// is what the truncation and snapshot paths rely on.
	rs.recomputeConfigLocked()

	rs.electionTimer = time.NewTimer(rs.electionTimeout)

	// The applier is owned by the state, not by RaftNode, because RaftState is
	// constructed on its own in tests and by callers that drive the RPC handlers
	// directly; those paths must still see committed entries reach applyCh.
	// Stop() (via RaftNode.Kill) is what ends it.
	rs.spawn(rs.applier)

	return rs, nil
}

// loadPersistentState loads the persistent state from storage
func (rs *RaftState) loadPersistentState() error {
	state, err := rs.persister.LoadRaftState()
	if err != nil {
		return err
	}

	if state != nil {
		rs.persistent = *state
		// Everything up to LastIncludedIndex was already applied and folded into
		// the snapshot before this node crashed, so the volatile indices must
		// start at the snapshot boundary rather than 0. Otherwise applyEntries
		// would try to re-read (already compacted) positions below the boundary
		// and either panic or re-apply entries that no longer exist in the log.
		rs.volatile.CommitIndex = state.LastIncludedIndex
		rs.volatile.LastApplied = state.LastIncludedIndex
		rs.logger.Printf("Loaded persistent state: term=%d, log_size=%d, lastIncludedIndex=%d",
			state.CurrentTerm, len(state.Log), state.LastIncludedIndex)
	}

	return nil
}

// persist saves the current persistent state to storage. It returns an error
// when the write fails so that callers on the RPC path can refuse to
// acknowledge a vote or term change that was not durably stored. This upholds
// the Raft rule (Figure 2) that persistent state must be written to stable
// storage before responding to RPCs.
func (rs *RaftState) persist() error {
	if rs.persister == nil {
		return nil
	}

	if err := rs.persister.SaveRaftState(&rs.persistent); err != nil {
		rs.logger.Printf("Error: failed to persist state: %v", err)
		return err
	}
	return nil
}

func (rs *RaftState) GetState() (int, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.persistent.CurrentTerm, rs.state == Leader
}

func (rs *RaftState) GetCurrentTerm() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.persistent.CurrentTerm
}

// GetCommitIndex returns the current commit index (an absolute log index).
// Exposed for observability and tests that assert commit progress across the
// log-compaction boundary.
func (rs *RaftState) GetCommitIndex() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.volatile.CommitIndex
}

// GetLastApplied returns the index of the last entry applied to the state
// machine (an absolute log index).
func (rs *RaftState) GetLastApplied() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.volatile.LastApplied
}

// GetCurrentLeader returns the leader this node currently believes in, or "" when
// it does not know of one. It is written under rs.mu by becomeFollowerLocked,
// becomeLeader and startElection, so it must be read under the lock too.
func (rs *RaftState) GetCurrentLeader() string {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.currentLeader
}

func (rs *RaftState) GetNodeState() NodeState {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.state
}

func (rs *RaftState) SetState(state NodeState) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.state = state

	if state == Leader {
		rs.initializeLeaderState()
	} else {
		rs.leader = nil
	}
}

// becomeFollowerLocked is the single follower transition. Every path that ends
// up a follower goes through it: a receiver accepting a leader's authority
// (AppendEntries, InstallSnapshot), a node that learns of a higher term
// (RequestVote and the three replication reply paths, plus ReadIndex's
// stepDown), and an election attempt that could not be made durable. Callers
// must hold rs.mu.
//
// It exists because those paths each did a slightly different subset of the
// work. The leader-side demotion paths set state/term/VotedFor and persisted but
// never re-armed the election timer that becomeLeader stopped, so a demoted
// leader sat as a follower with a dead timer and could not campaign again unless
// some other leader happened to reach it — a node lost to the cluster's
// availability for as long as that took (KNOWN_ISSUES.md R6).
//
// term is the term to adopt; passing the current term (or an older one) leaves
// the term and the vote untouched and performs only the role transition.
// leaderID is the leader we are now following, or "" when it is unknown.
//
// The returned error is the result of persisting a term change. Nothing is
// written when the term did not move, so the heartbeat path stays free of disk
// I/O. Callers decide what a failure means, keeping the discipline each path
// already had: an RPC handler must not answer under a term it has not durably
// recorded, while a reply path only logs. Either way the in-memory term stays
// bumped — the safe direction, since it can only stop us from acting in the
// older term (KNOWN_ISSUES.md 注記 3).
func (rs *RaftState) becomeFollowerLocked(term int, leaderID string) error {
	termChanged := term > rs.persistent.CurrentTerm
	if termChanged {
		rs.persistent.CurrentTerm = term
		rs.persistent.VotedFor = nil
	}

	rs.state = Follower
	rs.currentLeader = leaderID

	// A snapshot being received in chunks belongs to one leader in one term. Any
	// transition that changes either of those abandons it: the remaining chunks
	// will never arrive (a new leader restarts from offset 0), and buffering them
	// under a stale (term, leader) is how a half-installed snapshot from a
	// deposed leader could otherwise be completed. Same-term chunks from the same
	// leader survive, so an interleaved heartbeat does not restart the transfer.
	if rs.pendingChunks != nil &&
		(rs.pendingChunks.term != rs.persistent.CurrentTerm || rs.pendingChunks.leaderID != leaderID) {
		rs.discardSnapshotAssemblyLocked("term or leader changed")
	}

	// Per-peer replication state belongs to one leader term. Dropping it stops a
	// demoted leader from advancing MatchIndex on a reply that arrives late, and
	// makes "rs.leader != nil" mean "we are the leader" everywhere.
	rs.leader = nil
	// Always re-arm. This is the one place that knows the node is (or remains) a
	// follower, and the timer here is either stopped (we were the leader) or has
	// already fired (we are handling our own election timeout); a follower with
	// no armed timer never campaigns again.
	rs.resetElectionTimerLocked()

	if !termChanged {
		return nil
	}
	return rs.persist()
}

func (rs *RaftState) IncrementTerm() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.persistent.CurrentTerm++
	rs.persistent.VotedFor = nil
	if err := rs.persist(); err != nil {
		rs.logger.Printf("IncrementTerm: persist failed: %v", err)
	}
}

func (rs *RaftState) SetVotedFor(nodeID *string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.persistent.VotedFor = nodeID
	if err := rs.persist(); err != nil {
		rs.logger.Printf("SetVotedFor: persist failed: %v", err)
	}
}

func (rs *RaftState) GetVotedFor() *string {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.persistent.VotedFor
}

// ResetElectionTimer re-arms the election timer with a fresh random timeout. It
// takes rs.mu itself and must therefore NOT be called with the lock already
// held; the internal paths (all of which run under rs.mu) use
// resetElectionTimerLocked instead.
//
// The two used to be one lock-free function called from both sides: startElection
// released rs.mu before calling it while the RPC handlers called it under the
// lock, so the writes to electionTimeout/lastHeartbeat below raced
// (KNOWN_ISSUES.md E1).
func (rs *RaftState) ResetElectionTimer() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.resetElectionTimerLocked()
}

// resetElectionTimerLocked re-arms the election timer. Callers must hold rs.mu.
// time.Timer.Reset keeps using the same channel, so ElectionTimer()'s receiver
// needs no re-subscription.
func (rs *RaftState) resetElectionTimerLocked() {
	// Randomize election timeout on each reset to prevent split votes
	//nolint:gosec // G404: election timeout jitter does not need a crypto RNG
	randomTimeout := electionTimeoutBaseMs + rand.Intn(electionTimeoutJitterMs)
	rs.electionTimeout = time.Duration(randomTimeout) * time.Millisecond
	rs.electionTimer.Reset(rs.electionTimeout)
	rs.lastHeartbeat = time.Now()
}

func (rs *RaftState) ElectionTimer() <-chan time.Time {
	return rs.electionTimer.C
}

func (rs *RaftState) initializeLeaderState() {
	rs.leader = &LeaderState{
		NextIndex:  make(map[string]int),
		MatchIndex: make(map[string]int),
		inFlight:   make(map[string]bool),
	}

	nextIndex := rs.lastAbsLogIndex() + 1
	for _, peer := range rs.peerIDsLocked() {
		rs.leader.NextIndex[peer] = nextIndex
		rs.leader.MatchIndex[peer] = 0
	}
}

func (rs *RaftState) GetLeaderState() *LeaderState {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.leader
}

func (rs *RaftState) UpdateLastHeartbeat() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.lastHeartbeat = time.Now()
}

func (rs *RaftState) GetLastHeartbeat() time.Time {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.lastHeartbeat
}
