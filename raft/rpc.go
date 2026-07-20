package raft

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

type RequestVoteArgs struct {
	Term         int    `json:"term"`
	CandidateID  string `json:"candidateId"`
	LastLogIndex int    `json:"lastLogIndex"`
	LastLogTerm  int    `json:"lastLogTerm"`
}

type RequestVoteReply struct {
	Term        int  `json:"term"`
	VoteGranted bool `json:"voteGranted"`
}

type AppendEntriesArgs struct {
	Term         int        `json:"term"`
	LeaderID     string     `json:"leaderId"`
	PrevLogIndex int        `json:"prevLogIndex"`
	PrevLogTerm  int        `json:"prevLogTerm"`
	Entries      []LogEntry `json:"entries"`
	LeaderCommit int        `json:"leaderCommit"`
}

type AppendEntriesReply struct {
	Term    int  `json:"term"`
	Success bool `json:"success"`

	// Fast rollback optimization (Section 5.3)
	ConflictTerm  int `json:"conflictTerm,omitempty"`  // Term of conflicting entry
	ConflictIndex int `json:"conflictIndex,omitempty"` // First index of ConflictTerm
}

type InstallSnapshotArgs struct {
	Term              int    `json:"term"`
	LeaderID          string `json:"leaderId"`
	LastIncludedIndex int    `json:"lastIncludedIndex"`
	LastIncludedTerm  int    `json:"lastIncludedTerm"`
	Data              []byte `json:"data"`
}

type InstallSnapshotReply struct {
	Term int `json:"term"`
}

type RPCTransport interface {
	SendRequestVote(ctx context.Context, target string, args *RequestVoteArgs) (*RequestVoteReply, error)
	SendAppendEntries(ctx context.Context, target string, args *AppendEntriesArgs) (*AppendEntriesReply, error)
	SendInstallSnapshot(ctx context.Context, target string, args *InstallSnapshotArgs) (*InstallSnapshotReply, error)
}

func (rs *RaftState) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	reply.Term = rs.persistent.CurrentTerm
	reply.VoteGranted = false

	if args.Term < rs.persistent.CurrentTerm {
		return
	}

	// Track whether we mutated persistent state (term or vote) so we can flush
	// it to stable storage before replying.
	dirty := false
	if args.Term > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = args.Term
		rs.persistent.VotedFor = nil
		rs.state = Follower
		dirty = true
	}

	if rs.persistent.VotedFor == nil || *rs.persistent.VotedFor == args.CandidateID {
		// Evaluate the election restriction (§5.4.1) against the absolute last
		// log index/term, which after compaction is the snapshot boundary plus
		// the live log, not merely len(Log).
		lastLogIndex := rs.lastAbsLogIndex()
		lastLogTerm := rs.lastAbsLogTerm()

		if args.LastLogTerm > lastLogTerm ||
			(args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex) {
			rs.persistent.VotedFor = &args.CandidateID
			reply.VoteGranted = true
			rs.ResetElectionTimer()
			dirty = true
		}
	}

	// Persist the vote/term change before responding. If the write fails we must
	// not tell the candidate we voted for it: the vote is not durable, so a crash
	// here could let us vote again for a different candidate in the same term.
	// The in-memory VotedFor stays set, which is the safe direction (it only
	// prevents further votes this term); a later successful persist reconciles it.
	if dirty {
		if err := rs.persist(); err != nil {
			reply.VoteGranted = false
			rs.logger.Printf("RequestVote: refusing to grant vote, persist failed: %v", err)
		}
	}

	reply.Term = rs.persistent.CurrentTerm
}

func (rs *RaftState) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	reply.Term = rs.persistent.CurrentTerm
	reply.Success = false

	if args.Term < rs.persistent.CurrentTerm {
		return
	}

	termChanged := false
	if args.Term > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = args.Term
		rs.persistent.VotedFor = nil
		termChanged = true
	}

	rs.state = Follower
	rs.currentLeader = args.LeaderID // Track who the current leader is
	rs.ResetElectionTimer()

	// A term bump must reach stable storage before we respond. On failure we
	// leave Success=false and bail out rather than acknowledging under a term
	// we have not durably recorded.
	if termChanged {
		if err := rs.persist(); err != nil {
			rs.logger.Printf("AppendEntries: persist of term change failed: %v", err)
			reply.Term = rs.persistent.CurrentTerm
			return
		}
	}
	reply.Term = rs.persistent.CurrentTerm

	// Fast rollback optimization: handle log consistency check against the
	// (possibly compacted) log. All indices here are absolute.
	lastIdx := rs.lastAbsLogIndex()
	lii := rs.persistent.LastIncludedIndex
	switch {
	case args.PrevLogIndex > lastIdx:
		// Log is too short - return the absolute end so the leader can jump back.
		reply.ConflictTerm = -1
		reply.ConflictIndex = lastIdx + 1
		return
	case args.PrevLogIndex == lii:
		// PrevLogIndex sits exactly on our snapshot boundary (or the origin when
		// lii == 0). Its term is LastIncludedTerm by construction, so a mismatch
		// would mean the leader's committed prefix disagrees with our snapshot,
		// which Raft safety forbids. Nothing to verify; fall through to merge.
	case args.PrevLogIndex < lii:
		// PrevLogIndex refers to an entry our snapshot already subsumes. We can
		// no longer read that entry's term, but every index up to
		// LastIncludedIndex is committed and identical on all nodes, so the
		// prefix trivially matches. Accept and let mergeLogEntries skip the
		// entries that predate the boundary.
	default: // lii < PrevLogIndex <= lastIdx
		if rs.logTermAt(args.PrevLogIndex) != args.PrevLogTerm {
			// Term mismatch - find first index of the conflicting term, never
			// walking below the snapshot boundary (compacted terms are unknown).
			reply.ConflictTerm = rs.logTermAt(args.PrevLogIndex)
			conflictIndex := args.PrevLogIndex
			for conflictIndex > lii+1 && rs.logTermAt(conflictIndex-1) == reply.ConflictTerm {
				conflictIndex--
			}
			reply.ConflictIndex = conflictIndex
			return
		}
	}

	// Only persist/acknowledge if the merge actually changed the log; a delayed
	// or duplicated request whose entries already match is a no-op.
	if len(args.Entries) > 0 && rs.mergeLogEntries(args) {
		// The appended entries must be durable before we acknowledge them: a
		// leader that sees Success advances its commit index, so reporting
		// success for entries we could lose on a crash would break the log
		// matching guarantee.
		if err := rs.persist(); err != nil {
			rs.logger.Printf("AppendEntries: persist of log entries failed: %v", err)
			return
		}
	}

	if args.LeaderCommit > rs.volatile.CommitIndex {
		rs.volatile.CommitIndex = min(args.LeaderCommit, rs.lastAbsLogIndex())
		rs.applyEntries()
	}

	reply.Success = true
	reply.Term = rs.persistent.CurrentTerm
}

// mergeLogEntries merges the leader's entries into the follower's log following
// Raft §5.3 (receiver rules 3 & 4): an existing entry is deleted only when it
// conflicts with a new one (same index, different term); matching entries are
// left in place. This prevents a delayed or reordered AppendEntries from
// truncating a suffix the leader has already committed. It returns true only
// when the log was actually modified. Callers must hold rs.mu.
func (rs *RaftState) mergeLogEntries(args *AppendEntriesArgs) bool {
	lii := rs.persistent.LastIncludedIndex
	for i, entry := range args.Entries {
		absIndex := args.PrevLogIndex + i + 1 // absolute log index of this entry
		if absIndex <= lii {
			// Already subsumed by our snapshot; nothing to compare or write.
			continue
		}
		pos := rs.slicePos(absIndex) // position within the live log
		if pos < len(rs.persistent.Log) && rs.persistent.Log[pos].Term == entry.Term {
			continue // already present, no conflict
		}
		if pos < len(rs.persistent.Log) {
			// Conflicting term at this index: drop it and everything after.
			rs.persistent.Log = rs.persistent.Log[:pos]
		}
		rs.persistent.Log = append(rs.persistent.Log, args.Entries[i:]...)
		// Re-stamp absolute indices on the appended suffix.
		for j := pos; j < len(rs.persistent.Log); j++ {
			rs.persistent.Log[j].Index = lii + j + 1
		}
		return true
	}
	return false
}

func (rs *RaftState) startElection(transport RPCTransport) {
	rs.mu.Lock()
	rs.persistent.CurrentTerm++
	rs.state = Candidate
	rs.persistent.VotedFor = &rs.nodeID
	rs.currentLeader = "" // Clear current leader when starting election
	if err := rs.persist(); err != nil {
		// Could not durably record our candidacy (incremented term + self-vote).
		// Abort this election attempt; a later election timeout will retry once
		// storage recovers, rather than campaigning under an unpersisted term.
		rs.logger.Printf("startElection: persist failed, aborting election: %v", err)
		rs.state = Follower
		rs.mu.Unlock()
		// Re-arm the (already fired) election timer so we retry on the next
		// timeout once storage recovers; otherwise this node would never
		// campaign again until it hears from a leader.
		rs.ResetElectionTimer()
		return
	}
	currentTerm := rs.persistent.CurrentTerm
	// Advertise the absolute last log index/term so peers evaluate our
	// candidacy correctly across a compaction boundary (§5.4.1).
	lastLogIndex := rs.lastAbsLogIndex()
	lastLogTerm := rs.lastAbsLogTerm()

	// Use a vote counter that's protected by the RaftState mutex
	votes := 1
	votesNeeded := len(rs.peers)/quorumDivisor + 1
	rs.mu.Unlock()

	rs.ResetElectionTimer()

	// If this is a single-node cluster, immediately become leader
	if len(rs.peers) == 1 {
		rs.mu.Lock()
		rs.state = Leader
		rs.currentLeader = rs.nodeID // Set self as leader
		rs.initializeLeaderState()
		rs.mu.Unlock()
		// Stop election timer for leader
		rs.electionTimer.Stop()
		return
	}

	// Use a mutex to protect vote counting across goroutines
	var voteMu sync.Mutex

	for _, peer := range rs.peers {
		if peer == rs.nodeID {
			continue
		}

		go rs.requestVoteFromPeer(transport, peer, currentTerm, lastLogIndex, lastLogTerm, votesNeeded, &votes, &voteMu)
	}
}

// requestVoteFromPeer sends a single RequestVote RPC and, on a granted vote,
// promotes this node to Leader once a quorum is reached. Intended to run in
// its own goroutine.
func (rs *RaftState) requestVoteFromPeer(
	transport RPCTransport,
	peerID string,
	currentTerm, lastLogIndex, lastLogTerm, votesNeeded int,
	votes *int,
	voteMu *sync.Mutex,
) {
	args := &RequestVoteArgs{
		Term:         currentTerm,
		CandidateID:  rs.nodeID,
		LastLogIndex: lastLogIndex,
		LastLogTerm:  lastLogTerm,
	}

	ctx, cancel := context.WithTimeout(context.Background(), requestVoteTimeout)
	defer cancel()

	reply, err := transport.SendRequestVote(ctx, peerID, args)
	if err != nil {
		return
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.persistent.CurrentTerm != currentTerm || rs.state != Candidate {
		return
	}

	if reply.Term > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = reply.Term
		rs.state = Follower
		rs.persistent.VotedFor = nil
		if err := rs.persist(); err != nil {
			rs.logger.Printf("requestVoteFromPeer: persist of higher term failed: %v", err)
		}
		return
	}

	if reply.VoteGranted {
		voteMu.Lock()
		*votes++
		currentVotes := *votes
		voteMu.Unlock()

		if currentVotes >= votesNeeded && rs.state == Candidate {
			rs.state = Leader
			rs.currentLeader = rs.nodeID // Set self as leader
			rs.initializeLeaderState()
			// Stop election timer for leader
			rs.electionTimer.Stop()
		}
	}
}

func (rs *RaftState) sendHeartbeats(transport RPCTransport) {
	if rs.GetNodeState() != Leader {
		return
	}

	rs.mu.RLock()
	currentTerm := rs.persistent.CurrentTerm
	commitIndex := rs.volatile.CommitIndex
	isSingleNode := len(rs.peers) == 1
	totalPeers := len(rs.peers)
	rs.mu.RUnlock()

	// For single-node cluster, immediately commit any uncommitted entries
	// and confirm leadership (for read-only optimization)
	if isSingleNode {
		rs.mu.Lock()
		rs.updateCommitIndex()
		rs.lastLeaderConfirmation = time.Now()
		rs.mu.Unlock()
		return
	}

	// Track successful heartbeat responses for read-only optimization
	// Count starts at 1 because leader counts itself
	var successCount int32 = 1
	majority := totalPeers/quorumDivisor + 1
	var leaderConfirmed int32 = 0

	for _, peer := range rs.peers {
		if peer == rs.nodeID {
			continue
		}

		go rs.replicateToPeer(transport, peer, currentTerm, commitIndex, majority, &successCount, &leaderConfirmed)
	}
}

// replicateToPeer sends one round of replication to a single follower. It
// decides between AppendEntries and InstallSnapshot based on whether the
// follower's nextIndex still lies within the leader's (post-compaction) log.
// Intended to run in its own goroutine.
func (rs *RaftState) replicateToPeer(
	transport RPCTransport,
	peerID string,
	currentTerm, commitIndex, majority int,
	successCount, leaderConfirmed *int32,
) {
	rs.mu.RLock()
	nextIndex := rs.leader.NextIndex[peerID]
	lastIncludedIndex := rs.persistent.LastIncludedIndex
	lastIncludedTerm := rs.persistent.LastIncludedTerm
	snapshotter := rs.snapshotter

	// If nextIndex falls under the snapshot boundary, the entries this
	// follower needs have already been compacted away — fall through to
	// InstallSnapshot below. Otherwise translate the absolute prevLogIndex
	// into a slice position within the post-truncation log.
	sendSnapshot := false
	prevLogIndex := nextIndex - 1
	prevLogTerm := 0
	switch {
	case prevLogIndex == lastIncludedIndex:
		prevLogTerm = lastIncludedTerm
	case prevLogIndex > lastIncludedIndex:
		prevLogTerm = rs.persistent.Log[prevLogIndex-lastIncludedIndex-1].Term
	default:
		sendSnapshot = true
	}

	entries := make([]LogEntry, 0)
	if nextIndex > lastIncludedIndex {
		entries = rs.persistent.Log[nextIndex-lastIncludedIndex-1:]
	}

	rs.mu.RUnlock()

	if sendSnapshot {
		rs.sendSnapshotToPeer(transport, peerID, currentTerm, lastIncludedIndex, lastIncludedTerm, snapshotter)
		return
	}

	args := &AppendEntriesArgs{
		Term:         currentTerm,
		LeaderID:     rs.nodeID,
		PrevLogIndex: prevLogIndex,
		PrevLogTerm:  prevLogTerm,
		Entries:      entries,
		LeaderCommit: commitIndex,
	}

	ctx, cancel := context.WithTimeout(context.Background(), appendEntriesTimeout)
	defer cancel()

	reply, err := transport.SendAppendEntries(ctx, peerID, args)
	if err != nil {
		return
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if reply.Term > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = reply.Term
		rs.state = Follower
		rs.persistent.VotedFor = nil
		if err := rs.persist(); err != nil {
			rs.logger.Printf("replicateToPeer: persist of higher term failed: %v", err)
		}
		return
	}

	if rs.state != Leader || rs.persistent.CurrentTerm != currentTerm {
		return
	}

	if reply.Success {
		rs.leader.MatchIndex[peerID] = prevLogIndex + len(entries)
		rs.leader.NextIndex[peerID] = rs.leader.MatchIndex[peerID] + 1
		rs.updateCommitIndex()

		// Track successful response for read-only optimization
		newCount := atomic.AddInt32(successCount, 1)
		if int(newCount) >= majority && atomic.CompareAndSwapInt32(leaderConfirmed, 0, 1) {
			// Majority confirmed - update leader confirmation time
			rs.lastLeaderConfirmation = time.Now()
		}
	} else {
		rs.handleReplicationConflict(peerID, reply)
	}
}

// handleReplicationConflict applies the fast-rollback optimization (Section 5.3)
// to reset a follower's nextIndex using the conflict information the follower
// returned. Callers must hold rs.mu.
func (rs *RaftState) handleReplicationConflict(peerID string, reply *AppendEntriesReply) {
	if reply.ConflictTerm == -1 {
		// Follower's log is too short
		rs.leader.NextIndex[peerID] = reply.ConflictIndex
	} else {
		// Follower has a conflicting term
		// Search leader's log for the last entry with ConflictTerm
		lastIndexOfConflictTerm := -1
		for i := len(rs.persistent.Log) - 1; i >= 0; i-- {
			if rs.persistent.Log[i].Term == reply.ConflictTerm {
				lastIndexOfConflictTerm = rs.persistent.LastIncludedIndex + i + 1 // absolute index
				break
			}
		}

		if lastIndexOfConflictTerm > 0 {
			// Leader has entries from ConflictTerm, skip past them
			rs.leader.NextIndex[peerID] = lastIndexOfConflictTerm + 1
		} else {
			// Leader doesn't have ConflictTerm, use follower's ConflictIndex
			rs.leader.NextIndex[peerID] = reply.ConflictIndex
		}
	}

	// Ensure nextIndex doesn't go below 1
	if rs.leader.NextIndex[peerID] < 1 {
		rs.leader.NextIndex[peerID] = 1
	}
}

// sendSnapshotToPeer ships the current snapshot to a follower whose required
// entries have been compacted away, then advances that follower's match/next
// index on success. Intended to run in its own goroutine.
func (rs *RaftState) sendSnapshotToPeer(
	transport RPCTransport,
	peerID string,
	currentTerm, lastIncludedIndex, lastIncludedTerm int,
	snapshotter Snapshotter,
) {
	if snapshotter == nil {
		// Log compaction not wired up; nothing to send.
		return
	}
	data, err := snapshotter.ReadSnapshot()
	if err != nil || data == nil {
		return
	}
	snapArgs := &InstallSnapshotArgs{
		Term:              currentTerm,
		LeaderID:          rs.nodeID,
		LastIncludedIndex: lastIncludedIndex,
		LastIncludedTerm:  lastIncludedTerm,
		Data:              data,
	}
	ctx, cancel := context.WithTimeout(context.Background(), installSnapshotTimeout)
	defer cancel()
	reply, err := transport.SendInstallSnapshot(ctx, peerID, snapArgs)
	if err != nil {
		return
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if reply.Term > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = reply.Term
		rs.state = Follower
		rs.persistent.VotedFor = nil
		if err := rs.persist(); err != nil {
			rs.logger.Printf("sendSnapshotToPeer: persist of higher term failed: %v", err)
		}
		return
	}
	if rs.state != Leader || rs.persistent.CurrentTerm != currentTerm {
		return
	}
	// Follower has now installed the snapshot up to lastIncludedIndex.
	rs.leader.MatchIndex[peerID] = lastIncludedIndex
	rs.leader.NextIndex[peerID] = lastIncludedIndex + 1
}

func (rs *RaftState) updateCommitIndex() {
	if rs.state != Leader {
		return
	}

	for n := rs.volatile.CommitIndex + 1; n <= rs.lastAbsLogIndex(); n++ {
		if rs.logTermAt(n) != rs.persistent.CurrentTerm {
			continue
		}

		count := 1
		for _, peer := range rs.peers {
			if peer != rs.nodeID && rs.leader.MatchIndex[peer] >= n {
				count++
			}
		}

		if count*2 > len(rs.peers) {
			rs.volatile.CommitIndex = n
			rs.applyEntries()
		}
	}
}

func SerializeRequestVote(args *RequestVoteArgs) ([]byte, error) {
	return json.Marshal(args)
}

func DeserializeRequestVote(data []byte) (*RequestVoteArgs, error) {
	var args RequestVoteArgs
	err := json.Unmarshal(data, &args)
	return &args, err
}

func SerializeRequestVoteReply(reply *RequestVoteReply) ([]byte, error) {
	return json.Marshal(reply)
}

func DeserializeRequestVoteReply(data []byte) (*RequestVoteReply, error) {
	var reply RequestVoteReply
	err := json.Unmarshal(data, &reply)
	return &reply, err
}

func SerializeAppendEntries(args *AppendEntriesArgs) ([]byte, error) {
	return json.Marshal(args)
}

func DeserializeAppendEntries(data []byte) (*AppendEntriesArgs, error) {
	var args AppendEntriesArgs
	err := json.Unmarshal(data, &args)
	return &args, err
}

func SerializeAppendEntriesReply(reply *AppendEntriesReply) ([]byte, error) {
	return json.Marshal(reply)
}

func DeserializeAppendEntriesReply(data []byte) (*AppendEntriesReply, error) {
	var reply AppendEntriesReply
	err := json.Unmarshal(data, &reply)
	return &reply, err
}

// InstallSnapshot RPC handler
func (rs *RaftState) InstallSnapshot(args *InstallSnapshotArgs, reply *InstallSnapshotReply) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Reply immediately if term is stale
	if args.Term < rs.persistent.CurrentTerm {
		reply.Term = rs.persistent.CurrentTerm
		return
	}

	// Update term if necessary
	if args.Term > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = args.Term
		rs.persistent.VotedFor = nil
		rs.state = Follower
		if err := rs.persist(); err != nil {
			rs.logger.Printf("InstallSnapshot: persist of term change failed: %v", err)
			reply.Term = rs.persistent.CurrentTerm
			return
		}
	}

	reply.Term = rs.persistent.CurrentTerm

	// Reset election timer - valid communication from leader
	rs.state = Follower
	rs.currentLeader = args.LeaderID
	rs.ResetElectionTimer()

	// Don't install older snapshots
	if args.LastIncludedIndex <= rs.persistent.LastIncludedIndex {
		return
	}

	//  Discard log entries covered by snapshot
	newLog := make([]LogEntry, 0)
	for _, entry := range rs.persistent.Log {
		if entry.Index > args.LastIncludedIndex {
			newLog = append(newLog, entry)
		}
	}
	rs.persistent.Log = newLog

	// Update snapshot metadata
	rs.persistent.LastIncludedIndex = args.LastIncludedIndex
	rs.persistent.LastIncludedTerm = args.LastIncludedTerm

	// Update commit index and last applied
	if rs.volatile.CommitIndex < args.LastIncludedIndex {
		rs.volatile.CommitIndex = args.LastIncludedIndex
	}
	if rs.volatile.LastApplied < args.LastIncludedIndex {
		rs.volatile.LastApplied = args.LastIncludedIndex
	}

	// Persist the new snapshot boundary before handing the data to the state
	// machine. If this fails, do not apply: letting the state machine advance
	// past a snapshot index we did not durably record would leave the state
	// machine ahead of our Raft metadata after a crash.
	if err := rs.persist(); err != nil {
		rs.logger.Printf("InstallSnapshot: persist of snapshot metadata failed: %v", err)
		return
	}

	// Send snapshot data to apply channel for state machine to install
	rs.applyCh <- ApplyMsg{
		CommandValid:  false,
		Command:       args.Data,
		CommandIndex:  args.LastIncludedIndex,
		SnapshotValid: true,
		SnapshotIndex: args.LastIncludedIndex,
		SnapshotTerm:  args.LastIncludedTerm,
		SnapshotData:  args.Data,
	}
}

// Serialization for InstallSnapshot
func SerializeInstallSnapshotArgs(args *InstallSnapshotArgs) ([]byte, error) {
	return json.Marshal(args)
}

func DeserializeInstallSnapshotArgs(data []byte) (*InstallSnapshotArgs, error) {
	var args InstallSnapshotArgs
	err := json.Unmarshal(data, &args)
	return &args, err
}

func SerializeInstallSnapshotReply(reply *InstallSnapshotReply) ([]byte, error) {
	return json.Marshal(reply)
}

func DeserializeInstallSnapshotReply(data []byte) (*InstallSnapshotReply, error) {
	var reply InstallSnapshotReply
	err := json.Unmarshal(data, &reply)
	return &reply, err
}
