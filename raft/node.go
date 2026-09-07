package raft

import (
	"context"
	"log"
	"sync"
	"time"
)

type RaftNode struct {
	mu        sync.RWMutex
	state     *RaftState
	transport RPCTransport
	done      chan struct{}
	applyCh   chan ApplyMsg

	// killOnce keeps Kill idempotent: done is closed exactly once, while every
	// caller still waits for the goroutines through RaftState.Stop.
	killOnce sync.Once

	// peerAddrSink receives the cluster configuration's peer addresses when they
	// change; publishedAddrs is the last set handed to it. Both are guarded by
	// rn.mu and only touched from the event loop and the setter.
	peerAddrSink   func(map[string]string)
	publishedAddrs map[string]string

	logger *log.Logger
}

func NewRaftNode(nodeID string, peers []string, transport RPCTransport, applyCh chan ApplyMsg) *RaftNode {
	// A nil persister never loads state from disk, so construction cannot fail.
	node, _ := NewRaftNodeWithPersister(nodeID, peers, transport, applyCh, nil)
	return node
}

func NewRaftNodeWithPersister(
	nodeID string, peers []string, transport RPCTransport, applyCh chan ApplyMsg, persister Persister,
) (*RaftNode, error) {
	state, err := NewRaftStateWithPersister(nodeID, peers, applyCh, persister)
	if err != nil {
		// Refuse to construct/start the node when persistent state cannot be
		// trusted; the caller is expected to abort startup.
		return nil, err
	}

	node := &RaftNode{
		state:     state,
		transport: transport,
		done:      make(chan struct{}),
		applyCh:   applyCh,
		logger:    log.New(log.Writer(), "[RAFT-"+nodeID+"] ", log.LstdFlags),
	}

	// Started through the state's spawn helper so Kill joins the event loop along
	// with everything else it starts (KNOWN_ISSUES.md R19).
	state.spawn(node.run)
	return node, nil
}

func (rn *RaftNode) run() {
	ticker := time.NewTicker(raftTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-rn.done:
			return
		case <-rn.state.stopCh:
			// Also honor the state's stop signal, so the loop cannot outlive a
			// Stop that was reached by some route other than Kill.
			return
		case <-rn.state.ElectionTimer():
			rn.handleElectionTimeout()
		case <-ticker.C:
			rn.handleTick()
		}
	}
}

func (rn *RaftNode) handleElectionTimeout() {
	rn.mu.Lock()
	defer rn.mu.Unlock()

	if rn.state.GetNodeState() == Leader {
		return
	}
	// A server that is not a voter in the configuration it holds must not
	// campaign (paper §6). Two cases reach here: a server started with the
	// existing cluster's peer list so it can be added to it, which is not in any
	// configuration until C_old,new reaches its log; and a server that has been
	// removed but has not shut down yet. Neither can win, and both would raise
	// the cluster's term on every timeout if they tried. This is the weaker,
	// local half of the protection — the disruption check in RequestVote is what
	// protects the cluster from a server that campaigns anyway
	// (KNOWN_ISSUES.md R14).
	if !rn.state.IsVoter() {
		return
	}
	rn.logger.Printf("Election timeout, starting election for term %d", rn.state.GetCurrentTerm()+1)
	rn.state.startElection(rn.transport)
}

func (rn *RaftNode) handleTick() {
	rn.publishPeerAddresses()
	if rn.state.GetNodeState() == Leader {
		rn.state.sendHeartbeats(rn.transport)
	}
}

// publishPeerAddresses hands the current configuration's peer addresses to the
// registered sink whenever they have changed, so the transport's address book
// follows the cluster configuration instead of the -peers flag the process
// started with (KNOWN_ISSUES.md R14).
//
// It runs on the event loop rather than at the point the configuration changes,
// which keeps it off every lock-holding path: a configuration entry can land in
// an RPC handler under rs.mu, and calling out to the transport from there would
// invert the lock order between the Raft state and the transport's own mutex.
// The cost is that the address book lags a configuration change by at most one
// tick — harmless, since a peer that cannot be resolved yet is simply retried by
// the next replication round.
func (rn *RaftNode) publishPeerAddresses() {
	rn.mu.Lock()
	sink := rn.peerAddrSink
	rn.mu.Unlock()
	if sink == nil {
		return
	}

	addrs := rn.state.PeerAddresses()

	rn.mu.Lock()
	if samePeerAddresses(rn.publishedAddrs, addrs) {
		rn.mu.Unlock()
		return
	}
	rn.publishedAddrs = addrs
	rn.mu.Unlock()

	sink(addrs)
}

func samePeerAddresses(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if other, ok := b[k]; !ok || other != v {
			return false
		}
	}
	return true
}

// SetPeerAddressSink registers a callback that receives the addresses of the
// other servers in the cluster configuration whenever they change. main.go wires
// it to the transport's address book. The callback runs on the Raft event loop
// and must not block or call back into the node.
func (rn *RaftNode) SetPeerAddressSink(sink func(map[string]string)) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	rn.peerAddrSink = sink
	rn.publishedAddrs = nil
}

// SetPeerAddresses records the addresses of servers already in the cluster
// configuration whose address is not yet known — the fixed -peers startup path.
func (rn *RaftNode) SetPeerAddresses(addrs map[string]string) {
	rn.state.SetPeerAddresses(addrs)
}

// ProposeConfigChange starts a cluster configuration change (paper §6). It must
// be called on the leader; every other node returns ErrNotLeader so the HTTP
// layer can redirect. The returned configuration is the joint one that has just
// been appended — the change is complete only once C_new commits, which the
// leader drives on its own.
func (rn *RaftNode) ProposeConfigChange(add bool, nodeID, addr string) (*ClusterConfig, error) {
	return rn.state.ProposeConfigChange(add, nodeID, addr)
}

// GetClusterConfig returns the cluster configuration currently in effect on this
// node.
func (rn *RaftNode) GetClusterConfig() *ClusterConfig {
	return rn.state.GetClusterConfig()
}

func (rn *RaftNode) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) error {
	rn.logger.Printf("Received RequestVote from %s for term %d", args.CandidateID, args.Term)
	rn.state.RequestVote(args, reply)
	rn.logger.Printf("RequestVote reply: term=%d, granted=%v", reply.Term, reply.VoteGranted)
	return nil
}

func (rn *RaftNode) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) error {
	rn.logger.Printf("Received AppendEntries from %s for term %d", args.LeaderID, args.Term)
	rn.state.AppendEntries(args, reply)
	rn.logger.Printf("AppendEntries reply: term=%d, success=%v", reply.Term, reply.Success)
	return nil
}

// Start appends a client command to the leader's log. It returns the index the
// command was assigned, the leader's term, and whether this node is the leader.
// A non-nil error means the entry could not be made durable and was rolled back:
// the command was not started and must not be reported as accepted, even though
// isLeader is true.
//
// The leadership check and the append happen inside RaftState.Start, under a
// single rs.mu acquisition, so a concurrent demotion cannot slip between them
// (KNOWN_ISSUES.md R1). rn.mu is held only to read rn.logger safely against
// SetLogger; it does not order anything against rs.mu.
func (rn *RaftNode) Start(command interface{}) (index, term int, isLeader bool, err error) {
	rn.mu.Lock()
	defer rn.mu.Unlock()

	index, term, isLeader, err = rn.state.Start(command)
	switch {
	case !isLeader:
		return index, term, false, nil
	case err != nil:
		rn.logger.Printf("Start: failed to append command at term %d: %v", term, err)
		return index, term, true, err
	}
	rn.logger.Printf("Started command at index %d, term %d", index, term)

	return index, term, true, nil
}

func (rn *RaftNode) GetState() (int, bool) {
	return rn.state.GetState()
}

func (rn *RaftNode) GetNodeID() string {
	return rn.state.nodeID
}

func (rn *RaftNode) GetLeader() string {
	rn.mu.RLock()
	defer rn.mu.RUnlock()

	if rn.state.GetNodeState() == Leader {
		return rn.state.nodeID
	}

	// Return the tracked current leader. Read through the accessor: currentLeader
	// is written under rs.mu by the follower transition and the election paths,
	// and rn.mu orders nothing against it.
	return rn.state.GetCurrentLeader()
}

// Kill stops this node and blocks until every goroutine it started has returned:
// the event loop, the applier, in-flight replication and vote rounds, and the
// ReadIndex heartbeats. It is idempotent, and a second caller waits for the same
// set rather than returning early.
//
// It has to be synchronous because of what callers do next. Closing done and
// returning immediately (the previous behavior) left the tick handler and the
// replication replies running, so a kvs.Close() on the following line closed
// applyCh out from under an in-flight apply and the process died with "send on
// closed channel" — the flake behind TestFullSystemPersistence_CrashAndRecover
// in CI (KNOWN_ISSUES.md R19). After Kill returns, the applier has exited and
// the channel has no sender left, so closing it is safe.
//
// RPCs can still arrive after Kill — the transport is stopped separately, and in
// tests not at all. The handlers stay correct: they take rs.mu and update state
// as before, and the only thing they can no longer do is reach the state machine,
// because the applier is gone and every send is gated on the stop signal.
func (rn *RaftNode) Kill() {
	rn.killOnce.Do(func() {
		close(rn.done)
	})
	rn.state.Stop()
}

func (rn *RaftNode) GetLogLength() int {
	return rn.state.GetLastLogIndex()
}

func (rn *RaftNode) IsLeader() bool {
	_, isLeader := rn.state.GetState()
	return isLeader
}

// ReadIndex runs the ReadIndex protocol (Raft dissertation §6.4) and returns a
// commit index that is safe for a linearizable read once the caller's state
// machine has applied through it. It confirms current leadership with a fresh
// quorum of heartbeats and returns an error rather than a possibly stale index
// when this node is not the leader or cannot reach a quorum.
func (rn *RaftNode) ReadIndex() (int, error) {
	return rn.state.ReadIndex(rn.transport)
}

func (rn *RaftNode) GetRaftState() *RaftState {
	return rn.state
}

func (rn *RaftNode) SetLogger(logger *log.Logger) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	rn.logger = logger
}

// TriggerSnapshot is called by the state machine (kvstore) after it has
// persisted its own snapshot up to lastIncludedIndex. It schedules raft log
// truncation asynchronously so the apply loop is not blocked by disk IO.
//
// Concurrency: TruncateLogTo is idempotent and self-locks via rs.mu, so we
// do not need an extra mutex here. Stacked goroutines from rapid triggers
// will each acquire rs.mu in turn; the second-and-later calls become no-ops
// because absoluteIndex <= LastIncludedIndex by then. The compaction is spawned
// through the state so Kill waits for it too; after Kill nothing is started and
// the trigger is dropped, which is harmless — compaction is an optimization and
// the log is durable either way.
func (rn *RaftNode) TriggerSnapshot(lastIncludedIndex int) {
	rn.logger.Printf("Snapshot trigger received for index %d", lastIncludedIndex)
	rn.state.spawn(func() {
		if err := rn.state.TruncateLogTo(lastIncludedIndex); err != nil {
			rn.logger.Printf("Failed to truncate log at index %d: %v", lastIncludedIndex, err)
		}
	})
}

// SetSnapshotter delegates to the underlying RaftState. Callers (typically
// kvstore wiring in main.go) supply a Snapshotter so the leader can serve
// InstallSnapshot RPCs and so log compaction can persist state.
func (rn *RaftNode) SetSnapshotter(s Snapshotter) {
	rn.state.SetSnapshotter(s)
}

type MockTransport struct {
	nodes map[string]*RaftNode
	mu    sync.RWMutex
}

func NewMockTransport() *MockTransport {
	return &MockTransport{
		nodes: make(map[string]*RaftNode),
	}
}

func (mt *MockTransport) RegisterNode(nodeID string, node *RaftNode) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.nodes[nodeID] = node
}

func (mt *MockTransport) SendRequestVote(ctx context.Context, target string, args *RequestVoteArgs) (*RequestVoteReply, error) {
	mt.mu.RLock()
	node, exists := mt.nodes[target]
	mt.mu.RUnlock()

	if !exists {
		return nil, context.DeadlineExceeded
	}

	reply := &RequestVoteReply{}
	err := node.RequestVote(args, reply)
	return reply, err
}

func (mt *MockTransport) SendAppendEntries(ctx context.Context, target string, args *AppendEntriesArgs) (*AppendEntriesReply, error) {
	mt.mu.RLock()
	node, exists := mt.nodes[target]
	mt.mu.RUnlock()

	if !exists {
		return nil, context.DeadlineExceeded
	}

	reply := &AppendEntriesReply{}
	err := node.AppendEntries(args, reply)
	return reply, err
}

func (mt *MockTransport) SendInstallSnapshot(ctx context.Context, target string, args *InstallSnapshotArgs) (*InstallSnapshotReply, error) {
	mt.mu.RLock()
	node, exists := mt.nodes[target]
	mt.mu.RUnlock()

	if !exists {
		return nil, context.DeadlineExceeded
	}

	reply := &InstallSnapshotReply{}
	node.state.InstallSnapshot(args, reply)
	return reply, nil
}

func (mt *MockTransport) RemoveNode(nodeID string) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	delete(mt.nodes, nodeID)
}

func (mt *MockTransport) GetNodes() map[string]*RaftNode {
	mt.mu.RLock()
	defer mt.mu.RUnlock()

	result := make(map[string]*RaftNode)
	for k, v := range mt.nodes {
		result[k] = v
	}
	return result
}
