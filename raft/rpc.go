package raft

import (
	"context"
	"encoding/json"
	"sync"
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

// InstallSnapshotArgs carries one chunk of a snapshot (paper §7, Figure 13). A
// snapshot is shipped as a sequence of chunks that all describe the same
// generation — the same (LastIncludedIndex, LastIncludedTerm) — where Data holds
// the payload bytes starting at Offset and Done marks the last chunk. The
// receiver changes nothing until Done arrives.
//
// Offset and Done are omitempty, so the single-chunk form (Offset 0, Done true,
// the whole payload in Data) is the pre-chunking message plus `"done":true`, and
// the receiver treats it exactly as it treated the whole-payload message before.
// A message produced by a sender that predates chunking decodes as Offset 0 /
// Done false, which reads as "first chunk, more to come": the receiver buffers
// it and answers with the offset it expects next, so such a transfer stalls
// rather than installing a truncated snapshot. Every sender in this repository
// sets Done on its last (possibly only) chunk.
type InstallSnapshotArgs struct {
	Term              int    `json:"term"`
	LeaderID          string `json:"leaderId"`
	LastIncludedIndex int    `json:"lastIncludedIndex"`
	LastIncludedTerm  int    `json:"lastIncludedTerm"`
	// Offset is where Data belongs within the whole snapshot payload.
	Offset int `json:"offset,omitempty"`
	// Data is this chunk's slice of the payload, not the whole snapshot.
	Data []byte `json:"data"`
	// Done marks the final chunk of this snapshot.
	Done bool `json:"done,omitempty"`
}

// InstallSnapshotReply answers one chunk.
//
// Offset is the byte position the receiver expects in the next chunk. It is set
// when a chunk was buffered but the transfer is not finished (the length
// received so far), and when a chunk was refused because it did not continue
// what the receiver holds (0 to ask for a restart, or the length it holds if
// that chunk simply arrived out of order). It is not meaningful on the reply to
// a Done chunk.
type InstallSnapshotReply struct {
	Term   int `json:"term"`
	Offset int `json:"offset,omitempty"`
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

	// Adopt a higher term through the common follower transition. It persists the
	// term bump itself, so granting a vote below costs a second write; that only
	// happens on the election path, and it keeps the "durable before responding"
	// rule (Figure 2) in one place instead of spread across every caller.
	//
	// A failure here does not end the handler: the vote decision below still runs
	// and its persist covers the whole persistent state (term and vote together),
	// so a write that succeeds there makes both durable. The in-memory VotedFor
	// it may set is the safe direction either way (KNOWN_ISSUES.md 注記 3).
	if args.Term > rs.persistent.CurrentTerm {
		if err := rs.becomeFollowerLocked(args.Term, ""); err != nil {
			rs.logger.Printf("RequestVote: persist of higher term failed: %v", err)
		}
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
			rs.resetElectionTimerLocked()

			// Persist the vote before responding. If the write fails we must not
			// tell the candidate we voted for it: the vote is not durable, so a
			// crash here could let us vote again for a different candidate in the
			// same term. The in-memory VotedFor stays set, which is the safe
			// direction (it only prevents further votes this term); a later
			// successful persist reconciles it.
			if err := rs.persist(); err != nil {
				reply.VoteGranted = false
				rs.logger.Printf("RequestVote: refusing to grant vote, persist failed: %v", err)
			}
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

	// Accept this leader's authority through the common follower transition: it
	// adopts the term, records the leader, and re-arms the election timer, which
	// every valid AppendEntries must do. A term bump must reach stable storage
	// before we respond, so on a persist failure we leave Success=false and bail
	// out rather than acknowledging under a term we have not durably recorded.
	if err := rs.becomeFollowerLocked(args.Term, args.LeaderID); err != nil {
		rs.logger.Printf("AppendEntries: persist of term change failed: %v", err)
		reply.Term = rs.persistent.CurrentTerm
		return
	}
	reply.Term = rs.persistent.CurrentTerm

	// Fast rollback optimization: handle log consistency check against the
	// (possibly compacted) log. See checkLogConsistency for the cases.
	if !rs.checkLogConsistency(args, reply) {
		return
	}

	// Only persist/acknowledge if the merge actually changed the log; a delayed
	// or duplicated request whose entries already match is a no-op. That
	// optimization is only sound while the in-memory log and the on-disk log
	// agree, which is why the failure path below rolls the merge back.
	if len(args.Entries) > 0 {
		previousLog := rs.persistent.Log
		if rs.mergeLogEntries(args) {
			// The appended entries must be durable before we acknowledge them: a
			// leader that sees Success advances its commit index, so reporting
			// success for entries we could lose on a crash would break the log
			// matching guarantee.
			if err := rs.persist(); err != nil {
				// Roll the merge back (KNOWN_ISSUES.md R2). Keeping the
				// un-persisted entries in memory used to make the *retry* of this
				// very request look like a duplicate: mergeLogEntries compared the
				// resent entries against the in-memory copy, reported "already
				// present", skipped the persist entirely and answered Success=true.
				// The leader then counted this follower in MatchIndex and committed
				// entries that exist on no disk at all, so a crash here could lose a
				// committed entry (Figure 2 "before responding to RPCs", §5.4 Leader
				// Completeness). Restoring the previous slice header is enough
				// because mergeLogEntries never overwrites entries in place — it
				// caps the slice before appending, so the discarded entries stay
				// intact in the old backing array — and rs.mu is held throughout.
				rs.persistent.Log = previousLog

				// Tell the leader this was a transient storage failure, not a log
				// conflict. Left at their zero value, ConflictTerm/ConflictIndex
				// read to handleReplicationConflict as "conflicting term 0": it
				// finds no entry with term 0 in its own log, falls back to
				// ConflictIndex 0, and NextIndex gets clamped to 1 — resending this
				// follower's entire log on every heartbeat until storage recovers
				// (KNOWN_ISSUES.md R13-4). ConflictTerm=-1 with ConflictIndex set to
				// this request's own PrevLogIndex+1 asks the leader to retry from
				// exactly the position this request already tried, instead.
				reply.ConflictTerm = -1
				reply.ConflictIndex = args.PrevLogIndex + 1

				rs.logger.Printf("AppendEntries: persist of log entries failed, rolled back merge: %v", err)
				return
			}
		}
	}

	if args.LeaderCommit > rs.volatile.CommitIndex {
		// Figure 2, receiver rule 5: commitIndex = min(leaderCommit, index of
		// last new entry) — never min(leaderCommit, our whole log's end).
		// lastNewIndex is the last index *this* request actually vouches for; a
		// short request (fewer entries than our live log holds beyond
		// PrevLogIndex) must not let LeaderCommit reach into whatever suffix we
		// already had lying around. That suffix can be leftover from a term the
		// current leader knows nothing about — mergeLogEntries only overwrites it
		// on conflict, so an old, never-agreed-on tail can still be sitting past
		// where this request's Entries end (KNOWN_ISSUES.md R13-2).
		lastNewIndex := args.PrevLogIndex + len(args.Entries)
		newCommitIndex := min(args.LeaderCommit, lastNewIndex, rs.lastAbsLogIndex())
		if newCommitIndex > rs.volatile.CommitIndex {
			rs.volatile.CommitIndex = newCommitIndex
			rs.notifyApplierLocked()
		}
	}

	reply.Success = true
	reply.Term = rs.persistent.CurrentTerm
}

// checkLogConsistency implements the AppendEntries log consistency check
// (§5.3, receiver rule 2) against the (possibly compacted) log. All indices
// here are absolute. On a mismatch it fills in reply.ConflictTerm/ConflictIndex
// and returns false; a caller must treat false as "return without merging or
// advancing commit index". Callers must hold rs.mu.
func (rs *RaftState) checkLogConsistency(args *AppendEntriesArgs, reply *AppendEntriesReply) bool {
	lastIdx := rs.lastAbsLogIndex()
	lii := rs.persistent.LastIncludedIndex
	switch {
	case args.PrevLogIndex > lastIdx:
		// Log is too short - return the absolute end so the leader can jump back.
		reply.ConflictTerm = -1
		reply.ConflictIndex = lastIdx + 1
		return false
	case args.PrevLogIndex == lii:
		// PrevLogIndex sits exactly on our snapshot boundary (or the origin when
		// lii == 0). Its term is fixed at LastIncludedTerm by construction: every
		// node agrees on the term of a committed index, and everything up to and
		// including LastIncludedIndex is committed. A mismatch here is therefore
		// not an ordinary log divergence the fast-rollback optimization can walk
		// back through — it means the leader's committed prefix disagrees with
		// the one this node's snapshot was built from, a violation of Raft's
		// safety properties (State Machine Safety / Leader Completeness)
		// somewhere upstream of this handler. There is no repair available on
		// this receive path: forcing an InstallSnapshot would not help either,
		// since the R5 monotonicity guard (`args.LastIncludedIndex <=
		// LastIncludedIndex`, InstallSnapshot below) would refuse a snapshot at
		// an index we have already compacted to, so the leader could never push
		// one through. This rejects and keeps rejecting on every retry — a
		// follower stuck at this boundary is the safe failure mode; silently
		// accepting a history that disagrees with our own committed prefix is
		// not.
		if args.PrevLogTerm != rs.persistent.LastIncludedTerm {
			rs.logger.Printf("AppendEntries: leader's PrevLogTerm %d at our snapshot "+
				"boundary %d disagrees with our LastIncludedTerm %d; refusing "+
				"(committed-prefix mismatch, not routine log divergence, KNOWN_ISSUES.md R13-1)",
				args.PrevLogTerm, lii, rs.persistent.LastIncludedTerm)
			reply.ConflictTerm = -1
			reply.ConflictIndex = lii + 1
			return false
		}
		// Term matches; fall through to merge.
		return true
	case args.PrevLogIndex < lii:
		// PrevLogIndex refers to an entry our snapshot already subsumes. We can
		// no longer read that entry's term, but every index up to
		// LastIncludedIndex is committed and identical on all nodes, so the
		// prefix trivially matches — except when the leader's own Entries slice
		// happens to carry the boundary entry itself (absolute index == lii):
		// that term is directly comparable, and it must agree with
		// LastIncludedTerm for the same committed-prefix reason as the case
		// above.
		if boundaryOffset := lii - args.PrevLogIndex - 1; boundaryOffset >= 0 && boundaryOffset < len(args.Entries) {
			if boundaryTerm := args.Entries[boundaryOffset].Term; boundaryTerm != rs.persistent.LastIncludedTerm {
				rs.logger.Printf("AppendEntries: leader's entry at our snapshot "+
					"boundary %d carries term %d, disagreeing with our LastIncludedTerm %d; "+
					"refusing (committed-prefix mismatch, KNOWN_ISSUES.md R13-1)",
					lii, boundaryTerm, rs.persistent.LastIncludedTerm)
				reply.ConflictTerm = -1
				reply.ConflictIndex = lii + 1
				return false
			}
		}
		// Accept; mergeLogEntries skips the entries that predate the boundary.
		return true
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
			return false
		}
		return true
	}
}

// mergeLogEntries merges the leader's entries into the follower's log following
// Raft §5.3 (receiver rules 3 & 4): an existing entry is deleted only when it
// conflicts with a new one (same index, different term); matching entries are
// left in place. This prevents a delayed or reordered AppendEntries from
// truncating a suffix the leader has already committed. It returns true only
// when the log was actually modified. Callers must hold rs.mu.
//
// The merge never writes over an existing entry in place: on a conflict the log
// is re-sliced with its capacity capped at the conflict point, so the append
// that follows allocates a fresh backing array and the caller's saved slice
// header remains a valid snapshot of the pre-merge log. AppendEntries relies on
// that to roll the merge back when the persist fails (KNOWN_ISSUES.md R2).
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
			// Conflicting term at this index: drop it and everything after. The
			// capacity is capped at pos so the append below cannot overwrite the
			// dropped entries in the shared backing array — that keeps the
			// caller's pre-merge slice header usable as a rollback target.
			rs.persistent.Log = rs.persistent.Log[:pos:pos]
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
		// Drop back to follower through the common transition, which re-arms the
		// (already fired) election timer so we retry on the next timeout once
		// storage recovers; otherwise this node would never campaign again until
		// it hears from a leader. The term stays bumped in memory as on every
		// other unpersisted term change, so becomeFollowerLocked is passed the
		// term we already hold and writes nothing.
		if ferr := rs.becomeFollowerLocked(rs.persistent.CurrentTerm, ""); ferr != nil {
			rs.logger.Printf("startElection: follower transition after failed persist: %v", ferr)
		}
		rs.mu.Unlock()
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
	// Re-arm the (already fired) timer under the same lock the RPC handlers hold
	// when they reset it, so this election's timeout does not race with an
	// incoming AppendEntries (KNOWN_ISSUES.md E1). It also bounds this election:
	// if it draws no quorum, the timeout starts the next one.
	rs.resetElectionTimerLocked()
	rs.mu.Unlock()

	// If this is a single-node cluster, immediately become leader. becomeLeader
	// appends the current-term no-op and stops the election timer.
	if len(rs.peers) == 1 {
		rs.mu.Lock()
		rs.becomeLeader()
		rs.mu.Unlock()
		return
	}

	// Use a mutex to protect vote counting across goroutines
	var voteMu sync.Mutex

	for _, peer := range rs.peers {
		if peer == rs.nodeID {
			continue
		}

		// Spawned through rs.spawn so Kill can wait for it; after Stop nothing is
		// started and this election simply draws no votes (KNOWN_ISSUES.md R19).
		rs.spawn(func() {
			rs.requestVoteFromPeer(transport, peer, currentTerm, lastLogIndex, lastLogTerm, votesNeeded, &votes, &voteMu)
		})
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
		// Step down through the common transition, which also re-arms the election
		// timer (KNOWN_ISSUES.md R6). A failed persist is only logged here: there
		// is no RPC response riding on it, and the next election timeout retries.
		if err := rs.becomeFollowerLocked(reply.Term, ""); err != nil {
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
			// becomeLeader appends the current-term no-op, initializes leader
			// state, and stops the election timer. We already hold rs.mu.
			rs.becomeLeader()
		}
	}
}

func (rs *RaftState) sendHeartbeats(transport RPCTransport) {
	rs.mu.Lock()
	if rs.state != Leader || rs.leader == nil {
		rs.mu.Unlock()
		return
	}

	currentTerm := rs.persistent.CurrentTerm
	commitIndex := rs.volatile.CommitIndex

	// For a single-node cluster the leader is its own majority, so it can commit
	// outstanding entries directly.
	if len(rs.peers) == 1 {
		rs.updateCommitIndex()
		rs.mu.Unlock()
		return
	}

	// Claim each peer's replication slot before spawning, and skip the peers that
	// still have a round outstanding. Claiming under the same lock that reads the
	// term and commit index is what makes the serialization airtight: two ticks
	// cannot both see a peer idle.
	targets := make([]string, 0, len(rs.peers))
	for _, peer := range rs.peers {
		if peer == rs.nodeID || rs.leader.inFlight[peer] {
			continue
		}
		rs.leader.inFlight[peer] = true
		targets = append(targets, peer)
	}
	rs.mu.Unlock()

	for _, peer := range targets {
		// The slot was claimed above; if shutdown has already begun and nothing
		// is started, hand it back rather than leaving the peer marked busy.
		if !rs.spawn(func() { rs.replicatePeerOnce(transport, peer, currentTerm, commitIndex) }) {
			rs.releaseReplicationSlot(peer)
		}
	}
}

// replicatePeerOnce runs one replication round against peerID and releases that
// peer's slot when it returns, so the next tick can schedule another round.
//
// This is the one place a leader spawns replication work, which is what lets
// shutdown account for it: sendHeartbeats starts every round through rs.spawn,
// so Stop joins them all and Kill cannot return while a round is still running
// (KNOWN_ISSUES.md R19).
func (rs *RaftState) replicatePeerOnce(
	transport RPCTransport,
	peerID string,
	currentTerm, commitIndex int,
) {
	defer rs.releaseReplicationSlot(peerID)
	rs.replicateToPeer(transport, peerID, currentTerm, commitIndex)
}

// releaseReplicationSlot marks peerID as idle again. A demotion in the meantime
// drops the whole LeaderState, and a later election builds a fresh one, so there
// is nothing to release in that case.
func (rs *RaftState) releaseReplicationSlot(peerID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.leader != nil {
		delete(rs.leader.inFlight, peerID)
	}
}

// clampNextIndexIfStale corrects peerID's NextIndex down to one past the live
// log's end when it has drifted beyond it — the log was shortened (e.g.
// TruncateLogAfter) after NextIndex was last advanced. A no-op once the map
// already agrees, including when this node is no longer the leader.
func (rs *RaftState) clampNextIndexIfStale(peerID string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.leader == nil {
		return
	}
	if lastIdx := rs.lastAbsLogIndex(); rs.leader.NextIndex[peerID] > lastIdx+1 {
		rs.leader.NextIndex[peerID] = lastIdx + 1
	}
}

// replicateToPeer sends one round of replication to a single follower. It
// decides between AppendEntries and InstallSnapshot based on whether the
// follower's nextIndex still lies within the leader's (post-compaction) log.
// Intended to run in its own goroutine.
func (rs *RaftState) replicateToPeer(
	transport RPCTransport,
	peerID string,
	currentTerm, commitIndex int,
) {
	// nextIndex can be stale relative to a log that has since been shortened —
	// TruncateLogAfter running between the tick that scheduled this round (and
	// read NextIndex to decide whether a slot was free) and this goroutine's
	// turn to actually run. Left unclamped, the prevLogIndex computed below
	// could exceed the live log's end and the slice index derived from it would
	// run past len(Log): a panic (KNOWN_ISSUES.md R13-3, PR #26 followup).
	// Correcting the map entry itself — not just a local copy — needs its own
	// brief Lock: sendHeartbeats' inFlight claim already guarantees only one
	// round runs per peer at a time, so this cannot race the reply-processing
	// path's own writes to the same entry below, and it keeps a permanently
	// stale NextIndex from silently pinning every future round to zero new
	// entries (which a merely-local clamp would do, since it would recompute
	// the same "one past the log's current end" on every round without ever
	// correcting what sendHeartbeats/replicatePeerOnce actually schedules from).
	rs.clampNextIndexIfStale(peerID)

	rs.mu.RLock()
	if rs.state != Leader || rs.leader == nil {
		// Demoted between the tick that scheduled this round and now.
		// becomeFollowerLocked drops the per-peer replication state, so this is
		// also what keeps the read below from dereferencing a nil LeaderState.
		rs.mu.RUnlock()
		return
	}
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

	// Copy the entries out while still under the lock. The slice is handed to the
	// transport after RUnlock and read there — the HTTP transport JSON-marshals
	// it — while mergeLogEntries, TruncateLogAfter, TruncateLogTo and the
	// InstallSnapshot receive path all mutate persistent.Log's backing array.
	// Re-slicing alone is not enough: an append that fits in the spare capacity
	// writes through the shared array, so the marshaller could read an entry
	// mid-write (KNOWN_ISSUES.md E2). A shallow copy suffices — LogEntry.Command
	// is an interface{} the receiver only reads, never writes through.
	//
	// The cost is one copy of the outstanding suffix per replication round. That
	// suffix is unbounded today because there is no per-request entry cap;
	// introducing one (maxEntriesPerAppend) is a separate change and out of scope
	// here.
	entries := make([]LogEntry, 0)
	if nextIndex > lastIncludedIndex {
		src := rs.persistent.Log[nextIndex-lastIncludedIndex-1:]
		entries = make([]LogEntry, len(src))
		copy(entries, src)
	}

	rs.mu.RUnlock()

	if sendSnapshot {
		// Only nextIndex is carried across: it says what the follower still
		// needs. The boundary we sampled under the lock must not become the
		// RPC's LastIncludedIndex/LastIncludedTerm — those come from the
		// snapshot envelope itself (KNOWN_ISSUES.md R4).
		rs.sendSnapshotToPeer(transport, peerID, currentTerm, nextIndex, snapshotter)
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
		// Step down through the common transition, which also re-arms the election
		// timer becomeLeader stopped. Without that, a leader demoted by a reply
		// could never start an election again (KNOWN_ISSUES.md R6).
		if err := rs.becomeFollowerLocked(reply.Term, ""); err != nil {
			rs.logger.Printf("replicateToPeer: persist of higher term failed: %v", err)
		}
		return
	}

	if rs.state != Leader || rs.persistent.CurrentTerm != currentTerm {
		return
	}

	if reply.Success {
		// MatchIndex is a high-water mark, so never let a reply move it back.
		// Replies can still be processed out of order — a slow round answered
		// after a later, larger one — and rewinding MatchIndex here would rewind
		// NextIndex with it, re-sending entries the follower already acknowledged
		// and stalling the commit index behind a quorum that has in fact been
		// reached. The per-peer serialization above makes the reordering rare;
		// this makes it harmless.
		if matchIndex := prevLogIndex + len(entries); matchIndex > rs.leader.MatchIndex[peerID] {
			rs.leader.MatchIndex[peerID] = matchIndex
		}
		if next := rs.leader.MatchIndex[peerID] + 1; next > rs.leader.NextIndex[peerID] {
			rs.leader.NextIndex[peerID] = next
		}
		rs.updateCommitIndex()
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
//
// nextIndex is the follower's nextIndex as sampled by replicateToPeer; it is
// used only to decide whether the snapshot we can actually read is new enough
// to help. Everything the RPC asserts about the snapshot — its index, its term
// and its bytes — comes from the single envelope returned by ReadSnapshot.
// Sampling the boundary under rs.mu and the payload outside it (the previous
// behavior) let a concurrent compaction or InstallSnapshot rewrite snapshot.json
// in between, so the follower could be told that generation B's bytes belonged
// at generation A's (index, term) — a state machine silently installed under
// the wrong log position (KNOWN_ISSUES.md R4).
//
// The envelope goes out as a sequence of chunks (KNOWN_ISSUES.md R15); all of
// them describe that one envelope, so the R4 property is unaffected by the
// split. The match/next index only moves once the follower has acknowledged the
// final chunk, because only then has it installed anything.
func (rs *RaftState) sendSnapshotToPeer(
	transport RPCTransport,
	peerID string,
	currentTerm, nextIndex int,
	snapshotter Snapshotter,
) {
	if snapshotter == nil {
		// Log compaction not wired up; nothing to send.
		return
	}
	snapshot, err := snapshotter.ReadSnapshot()
	if err != nil || snapshot == nil {
		return
	}
	// A snapshot that ends before the entry this follower already has cannot
	// move it forward, and installing it would drag its match index backwards.
	// This means our own snapshot file is behind the boundary we compacted to,
	// so wait for the next generation rather than shipping a useless payload.
	if snapshot.LastIncludedIndex < nextIndex-1 {
		rs.logger.Printf("sendSnapshotToPeer: snapshot at index %d is older than %s's nextIndex %d; not sending",
			snapshot.LastIncludedIndex, peerID, nextIndex)
		return
	}
	if !rs.streamSnapshotToPeer(transport, peerID, currentTerm, snapshot) {
		return
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.state != Leader || rs.persistent.CurrentTerm != currentTerm {
		return
	}
	// Follower has now installed the snapshot we actually shipped, so its match
	// index follows that envelope's boundary — not the boundary we happened to
	// read from our own state before the send. Monotonic for the same reason as
	// the AppendEntries path: a slow InstallSnapshot acknowledged after the
	// follower has already been caught up further must not drag it back.
	if snapshot.LastIncludedIndex > rs.leader.MatchIndex[peerID] {
		rs.leader.MatchIndex[peerID] = snapshot.LastIncludedIndex
	}
	if next := rs.leader.MatchIndex[peerID] + 1; next > rs.leader.NextIndex[peerID] {
		rs.leader.NextIndex[peerID] = next
	}
}

// streamSnapshotToPeer sends one snapshot envelope to peerID as a sequence of
// chunks and reports whether the follower acknowledged all of them — i.e.
// whether it has installed the snapshot. Every chunk names the same envelope, so
// the follower can only assemble one generation (KNOWN_ISSUES.md R4/R15).
//
// The whole transfer runs inside this peer's single in-flight replication slot,
// so a snapshot still blocks heartbeats to that follower for its duration. What
// chunking changes is that the slot is now held by a series of short RPCs rather
// than one long one, and the checks between them let the transfer be abandoned
// promptly instead of after a five-second timeout. Freeing the slot entirely
// during a snapshot is a separate change (PR #26 follow-up) and out of scope
// here.
//
// Between chunks, and never in the middle of one, the transfer is abandoned when
// shutdown has begun or this node is no longer the leader of currentTerm.
// Abandoning costs nothing on the receiver: it has written nothing.
//
// There is no resume. A failed chunk ends the round, and the next tick's
// replicateToPeer starts again from offset 0 — with a *freshly read* envelope,
// which by then may be a newer generation than the one abandoned. Resuming
// mid-payload would mean the leader remembering, per peer, both an offset and
// the generation that offset belongs to, and re-reading the same generation
// later even after a compaction has replaced it. Restarting is what keeps "one
// transfer, one envelope" true without any of that state, and the receiver's
// offset check is what makes it safe: a stray chunk from the abandoned round
// cannot continue the new one, because its offset will not match.
func (rs *RaftState) streamSnapshotToPeer(
	transport RPCTransport,
	peerID string,
	currentTerm int,
	snapshot *SnapshotData,
) bool {
	chunkSize := rs.getSnapshotChunkSize()

	for offset := 0; ; {
		end := min(offset+chunkSize, len(snapshot.Data))
		// A zero-length payload still describes a boundary the follower must
		// install, so it travels as one empty chunk with Done set rather than as
		// no RPC at all.
		done := end == len(snapshot.Data)

		select {
		case <-rs.stopCh:
			return false
		default:
		}
		if !rs.isLeaderInTerm(currentTerm) {
			return false
		}

		reply, err := rs.sendSnapshotChunk(transport, peerID, currentTerm, snapshot, offset, end, done)
		if err != nil {
			rs.logger.Printf("sendSnapshotToPeer: chunk at offset %d of the snapshot at index %d "+
				"failed for %s, abandoning this round: %v",
				offset, snapshot.LastIncludedIndex, peerID, err)
			return false
		}

		if rs.stepDownIfHigherTerm(reply.Term, "sendSnapshotToPeer") {
			return false
		}
		if !rs.isLeaderInTerm(currentTerm) {
			return false
		}

		if done {
			return true
		}

		if reply.Offset != end {
			// The follower did not take this chunk (it refused the offset, or the
			// snapshot is no longer newer than what it has applied). Stop rather
			// than push the rest of a payload it is not assembling.
			rs.logger.Printf("sendSnapshotToPeer: %s expects offset %d after our chunk ending at %d; "+
				"abandoning this round", peerID, reply.Offset, end)
			return false
		}
		offset = end
	}
}

// sendSnapshotChunk sends the payload bytes [offset, end) as one RPC.
func (rs *RaftState) sendSnapshotChunk(
	transport RPCTransport,
	peerID string,
	currentTerm int,
	snapshot *SnapshotData,
	offset, end int,
	done bool,
) (*InstallSnapshotReply, error) {
	args := &InstallSnapshotArgs{
		Term:              currentTerm,
		LeaderID:          rs.nodeID,
		LastIncludedIndex: snapshot.LastIncludedIndex,
		LastIncludedTerm:  snapshot.LastIncludedTerm,
		Offset:            offset,
		Data:              snapshot.Data[offset:end],
		Done:              done,
	}

	// The timeout bounds one chunk, which is the point of chunking: a slow link
	// no longer has to move a whole snapshot within a single RPC deadline.
	ctx, cancel := context.WithTimeout(context.Background(), installSnapshotTimeout)
	defer cancel()
	return transport.SendInstallSnapshot(ctx, peerID, args)
}

// isLeaderInTerm reports whether this node is still the leader of term.
func (rs *RaftState) isLeaderInTerm(term int) bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.state == Leader && rs.persistent.CurrentTerm == term
}

// stepDownIfHigherTerm demotes this node when a reply carries a newer term, and
// reports whether it did. The demotion goes through the common transition, which
// also re-arms the election timer becomeLeader stopped (KNOWN_ISSUES.md R6). A
// failed persist is only logged: no RPC response rides on it, and the next
// election timeout retries.
func (rs *RaftState) stepDownIfHigherTerm(replyTerm int, where string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if replyTerm <= rs.persistent.CurrentTerm {
		return false
	}
	if err := rs.becomeFollowerLocked(replyTerm, ""); err != nil {
		rs.logger.Printf("%s: persist of higher term failed: %v", where, err)
	}
	return true
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
			rs.notifyApplierLocked()
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

// InstallSnapshot RPC handler (paper §7, Figure 13 receiver rules).
//
// Durability ordering invariant — do not reorder these three steps:
//
//  1. the state machine payload becomes durable (Snapshotter.InstallSnapshot),
//  2. the Raft snapshot boundary becomes durable (persist),
//  3. the in-memory state machine is updated (the applyCh send).
//
// The two files have no cross-file atomicity, so a crash always lands between
// two of these steps and the order decides which way the inconsistency falls
// (KNOWN_ISSUES.md R3). With payload first, a crash can only leave snapshot.json
// ahead of raft_state.json, which is recoverable: the node comes up with the
// older Raft boundary, replays the log, and the state machine ignores everything
// at or below the index its snapshot already covers. The previous order —
// boundary first, payload later on the apply path — produced the opposite and
// unrecoverable state: raft_state.json claimed CommitIndex/LastApplied = N with
// its log discarded below N, while the state machine on disk was still at some
// index < N and could never be sent those entries again.
//
// The applyCh send now happens on the applier goroutine (KNOWN_ISSUES.md B3),
// which does not change the invariant: steps 1 and 2 still complete, in that
// order, inside this handler, and only then is the snapshot queued for the
// applier. What step 3 becomes is "queue the in-memory update"; it is no longer
// this goroutine that waits for the state machine to take it.
//
// Chunked transfer (KNOWN_ISSUES.md R15) leaves that invariant alone by keeping
// a partial transfer entirely private. A snapshot arrives as a sequence of
// chunks (Figure 13 receiver rules 2-4) and everything before the final one only
// appends to an in-memory buffer: no Raft state, no state machine, no file on
// disk reflects a snapshot that has not finished arriving. So the three steps
// above still run exactly once per snapshot, in one critical section, with the
// assembled payload standing in for args.Data. What every chunk does do is the
// term handling and the election-timer reset above them — a multi-chunk transfer
// is a live leader talking to us, and must not look like silence.
func (rs *RaftState) InstallSnapshot(args *InstallSnapshotArgs, reply *InstallSnapshotReply) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Receiver rule 1: reply immediately if term is stale. Note this leaves any
	// transfer in progress untouched — a straggling chunk from a deposed leader
	// cannot disturb the one we are assembling for the current one.
	if args.Term < rs.persistent.CurrentTerm {
		reply.Term = rs.persistent.CurrentTerm
		return
	}

	// Accept this leader's authority through the common follower transition: it
	// adopts the term, records the leader and resets the election timer (valid
	// communication from a leader). A term bump must be durable before we answer,
	// so a persist failure ends the handler before anything else is touched —
	// which also keeps this ahead of the ordering invariant's step 1.
	if err := rs.becomeFollowerLocked(args.Term, args.LeaderID); err != nil {
		rs.logger.Printf("InstallSnapshot: persist of term change failed: %v", err)
		reply.Term = rs.persistent.CurrentTerm
		return
	}

	reply.Term = rs.persistent.CurrentTerm

	// Refuse any snapshot that would move this node backwards. Comparing only
	// against LastIncludedIndex (the previous behavior) let a delayed or
	// duplicated RPC carrying an old snapshot through whenever this node had
	// applied past that index without compacting to it: the log was then cut
	// back to the stale boundary and the payload was handed to the state
	// machine, which replaced already-applied state with an earlier version
	// (KNOWN_ISSUES.md R5). LastApplied is never below LastIncludedIndex on the
	// normal paths, but both are checked because TruncateLogTo can advance the
	// boundary independently.
	//
	// Checked on every chunk, not just the first. LastApplied only grows, so a
	// transfer that was worth starting can become pointless while it is still
	// arriving (the missing entries reached us as ordinary AppendEntries, say);
	// refusing here ends it at the next chunk instead of at the last one, and
	// drops the buffer with it.
	if args.LastIncludedIndex <= rs.volatile.LastApplied ||
		args.LastIncludedIndex <= rs.persistent.LastIncludedIndex {
		rs.logger.Printf("InstallSnapshot: ignoring snapshot at index %d (lastApplied=%d, lastIncluded=%d)",
			args.LastIncludedIndex, rs.volatile.LastApplied, rs.persistent.LastIncludedIndex)
		if rs.pendingChunks != nil && rs.pendingChunks.lastIncludedIndex == args.LastIncludedIndex {
			rs.discardSnapshotAssemblyLocked("snapshot no longer newer than our applied state")
		}
		return
	}

	// Receiver rules 2-4: buffer this chunk. Until the transfer is complete this
	// is the whole of the handler's effect — the durability ordering below runs
	// once, for the assembled payload.
	payload, complete := rs.acceptSnapshotChunkLocked(args, reply)
	if !complete {
		return
	}

	// Step 1: make the state machine payload durable first. Until this write
	// lands, nothing about our Raft state may change — a boundary recorded
	// against a payload that is not on disk is the unrecoverable direction.
	// With no snapshotter wired (memory-only configurations, and tests) the
	// state machine still owns the write and gets told so via the ApplyMsg.
	snapshotPersisted := false
	if rs.snapshotter != nil {
		if err := rs.snapshotter.InstallSnapshot(
			payload, args.LastIncludedIndex, args.LastIncludedTerm,
		); err != nil {
			rs.logger.Printf("InstallSnapshot: persisting the snapshot payload failed, "+
				"leaving raft state untouched: %v", err)
			return
		}
		snapshotPersisted = true
	}

	// Step 2: advance the Raft boundary and make it durable. Everything the
	// persist covers is saved first so a failed write can be undone exactly:
	// logAfterSnapshot only re-slices (or drops) the log, so restoring the slice
	// header restores the entries, and rs.mu is held throughout.
	prevLog := rs.persistent.Log
	prevLastIncludedIndex := rs.persistent.LastIncludedIndex
	prevLastIncludedTerm := rs.persistent.LastIncludedTerm
	prevCommitIndex := rs.volatile.CommitIndex
	prevLastApplied := rs.volatile.LastApplied

	// Replace the log per the paper's §7 retention rule: entries the snapshot
	// covers always go, and the suffix above the boundary survives only when
	// our entry at LastIncludedIndex agrees with the snapshot's term. Must run
	// before LastIncludedIndex is advanced below — the rule is evaluated
	// against the log we still hold.
	rs.persistent.Log = rs.logAfterSnapshot(args.LastIncludedIndex, args.LastIncludedTerm)

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

	if err := rs.persist(); err != nil {
		// Roll the whole install back so memory and disk still agree when this
		// handler returns, the same discipline AppendEntries follows for a
		// failed merge (KNOWN_ISSUES.md R2). The payload we already wrote stays
		// on disk; that only leaves snapshot.json ahead of raft_state.json,
		// which the startup check and the state machine's monotonicity guard
		// absorb. The leader retries and the install completes then.
		rs.persistent.Log = prevLog
		rs.persistent.LastIncludedIndex = prevLastIncludedIndex
		rs.persistent.LastIncludedTerm = prevLastIncludedTerm
		rs.volatile.CommitIndex = prevCommitIndex
		rs.volatile.LastApplied = prevLastApplied
		rs.logger.Printf("InstallSnapshot: persist of snapshot metadata failed, rolled back: %v", err)
		return
	}

	// Step 3: hand the snapshot to the state machine for its in-memory update.
	// Queued for the applier rather than sent from here, so a state machine that
	// is slow to take it does not hold rs.mu — and with it every RPC and the
	// election timer — for the duration (KNOWN_ISSUES.md B3).
	//
	// The queue slot is claimed under the same lock that just advanced
	// LastApplied to this snapshot's index, which is what orders the two streams:
	// no command above this index can have been claimed by the applier yet, so
	// the snapshot reaches the state machine before all of them.
	rs.pendingSnapshot = &ApplyMsg{
		CommandValid:      false,
		Command:           payload,
		CommandIndex:      args.LastIncludedIndex,
		SnapshotValid:     true,
		SnapshotIndex:     args.LastIncludedIndex,
		SnapshotTerm:      args.LastIncludedTerm,
		SnapshotData:      payload,
		SnapshotPersisted: snapshotPersisted,
	}
	rs.notifyApplierLocked()
}

// acceptSnapshotChunkLocked implements Figure 13's receiver rules 2-4 against
// rs.pendingChunks and returns the complete payload once the final chunk has
// arrived. Callers must hold rs.mu.
//
//  2. If offset is 0, create a new snapshot file
//  3. Write data into the snapshot file at the given offset
//  4. Reply and wait for more data chunks if done is false
//
// The "snapshot file" here is a buffer in memory, not a file: the payload is
// written to disk in one atomic write by Snapshotter.InstallSnapshot when the
// transfer completes, which is what lets the durability ordering invariant on
// InstallSnapshot hold as a single step. The cost is that the receiver still
// holds a whole snapshot in memory; what chunking bounds is the size of one RPC
// (and so how long a single RPC occupies the leader's per-peer slot), not the
// receiver's peak memory.
//
// A chunk that does not continue what we hold is refused rather than merged.
// reply.Offset then names the offset that would be accepted — the length
// buffered so far when this is simply a chunk out of order, or 0 when there is
// nothing here to continue and the leader should start again. Refusing keeps
// rule 3's "at the given offset" honest without needing to track holes: the
// buffer is always exactly the payload's first len(buf) bytes.
func (rs *RaftState) acceptSnapshotChunkLocked(
	args *InstallSnapshotArgs, reply *InstallSnapshotReply,
) (payload []byte, complete bool) {
	if args.Offset == 0 {
		// Rule 2. Any earlier attempt is superseded: a leader that starts over
		// does so from the beginning, and its new chunks may well belong to a
		// newer snapshot generation than the bytes we were collecting.
		rs.discardSnapshotAssemblyLocked("leader restarted the transfer at offset 0")

		if args.Done {
			// The single-chunk form — the whole snapshot in one RPC, which is
			// what every sender produced before chunking existed. Nothing needs
			// buffering, so this path behaves exactly as it always did.
			return args.Data, true
		}

		// Copy the chunk out: the mock transport hands the sender's own slice
		// straight to this handler, and the sender re-slices it from the snapshot
		// envelope it is walking through.
		rs.pendingChunks = &snapshotAssembly{
			leaderID:          args.LeaderID,
			term:              rs.persistent.CurrentTerm,
			lastIncludedIndex: args.LastIncludedIndex,
			lastIncludedTerm:  args.LastIncludedTerm,
			buf:               append([]byte(nil), args.Data...),
		}
		reply.Offset = len(rs.pendingChunks.buf) // rule 4
		return nil, false
	}

	assembly := rs.pendingChunks
	if assembly == nil || !assembly.matches(args, rs.persistent.CurrentTerm) {
		rs.logger.Printf("InstallSnapshot: chunk at offset %d for the snapshot at index %d "+
			"continues nothing we hold; asking %s to restart from offset 0",
			args.Offset, args.LastIncludedIndex, args.LeaderID)
		reply.Offset = 0
		return nil, false
	}

	if args.Offset != len(assembly.buf) {
		// A gap would leave a hole in the payload and an overlap would duplicate
		// bytes. Keep what we have — this chunk arrived out of order, which says
		// nothing against the prefix already buffered — and name the offset that
		// fits.
		rs.logger.Printf("InstallSnapshot: chunk at offset %d does not continue the %d bytes "+
			"buffered for the snapshot at index %d; expecting offset %d",
			args.Offset, len(assembly.buf), args.LastIncludedIndex, len(assembly.buf))
		reply.Offset = len(assembly.buf)
		return nil, false
	}

	assembly.buf = append(assembly.buf, args.Data...) // rule 3

	if !args.Done {
		reply.Offset = len(assembly.buf) // rule 4
		return nil, false
	}

	payload = assembly.buf
	rs.pendingChunks = nil
	return payload, true
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
