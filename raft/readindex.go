package raft

import (
	"context"
	"errors"
)

// Errors returned by ReadIndex.
var (
	// ErrNotLeader is returned when a read is attempted on a node that is not the
	// leader. Its message contains "not leader" so the HTTP layer maps it to the
	// same 503 leader-redirect used by the write path.
	ErrNotLeader = errors.New("not leader")
	// ErrNoCurrentTermCommit is returned before the leader has committed an entry
	// in its current term. Immediately after an election the no-op has been
	// appended but not yet committed; reads must wait until it commits to be
	// linearizable.
	ErrNoCurrentTermCommit = errors.New("no committed entry in current term yet")
	// ErrLeadershipNotConfirmed is returned when a quorum of heartbeat ACKs could
	// not be gathered, so this node cannot prove it is still the leader for the
	// read index it captured. Refusing the read here is precisely what stops a
	// partitioned ex-leader from serving stale data — the failure mode of the
	// lease-based optimization this replaces.
	ErrLeadershipNotConfirmed = errors.New("leadership not confirmed by quorum")
)

// hasCurrentTermCommittedLocked reports whether the entry at the commit index
// belongs to the current term, i.e. the leader has committed something in this
// term. Callers must hold rs.mu. logTermAt resolves the term across the
// compaction boundary (commit index never drops below LastIncludedIndex).
func (rs *RaftState) hasCurrentTermCommittedLocked() bool {
	ci := rs.volatile.CommitIndex
	return ci > 0 && rs.logTermAt(ci) == rs.persistent.CurrentTerm
}

// ReadIndex implements the ReadIndex protocol for linearizable reads (Raft
// dissertation §6.4). It returns a commit index that is safe to read once the
// caller's state machine has applied through it. The returned index is only
// valid after this method has:
//
//  1. confirmed this node is the leader and has committed an entry in the
//     current term (guaranteed shortly after election by the no-op);
//  2. captured the current commit index as the read index; and
//  3. exchanged a round of heartbeats and received current-term ACKs from a
//     quorum (including itself), proving no other leader has superseded it at
//     the instant the read index was captured.
//
// On any failure it returns a zero index and a non-nil error and never a
// possibly stale value. A single-node cluster is its own quorum and returns
// immediately.
func (rs *RaftState) ReadIndex(transport RPCTransport) (int, error) {
	rs.mu.RLock()
	if rs.state != Leader {
		rs.mu.RUnlock()
		return 0, ErrNotLeader
	}
	currentTerm := rs.persistent.CurrentTerm
	readIndex := rs.volatile.CommitIndex
	if !rs.hasCurrentTermCommittedLocked() {
		rs.mu.RUnlock()
		return 0, ErrNoCurrentTermCommit
	}
	if len(rs.peers) == 1 {
		rs.mu.RUnlock()
		return readIndex, nil
	}
	// Build a heartbeat consistent with the leader's own log tip. Followers that
	// are behind reject on the log check but still answer with the current term,
	// which is all leadership confirmation requires.
	prevLogIndex := rs.lastAbsLogIndex()
	prevLogTerm := rs.lastAbsLogTerm()
	peers := make([]string, 0, len(rs.peers))
	for _, p := range rs.peers {
		if p != rs.nodeID {
			peers = append(peers, p)
		}
	}
	majority := len(rs.peers)/quorumDivisor + 1
	rs.mu.RUnlock()

	if rs.confirmLeadership(transport, currentTerm, readIndex, prevLogIndex, prevLogTerm, peers, majority) {
		return readIndex, nil
	}
	// confirmLeadership steps down if it observed a higher term; distinguish that
	// (not leader anymore) from a plain quorum-not-reached so the HTTP layer can
	// redirect on the former.
	if _, isLeader := rs.GetState(); !isLeader {
		return 0, ErrNotLeader
	}
	return 0, ErrLeadershipNotConfirmed
}

// confirmLeadership sends an empty AppendEntries (heartbeat) to every peer in
// parallel and reports whether a quorum (counting this node) answered with the
// current term. A reply carrying a higher term triggers a step down and a false
// result. Only the reply term matters here: a follower that recognizes us as
// leader for currentTerm cannot have granted a vote to another leader in that
// term, so a current-term quorum proves we are still the sole leader.
func (rs *RaftState) confirmLeadership(
	transport RPCTransport,
	currentTerm, leaderCommit, prevLogIndex, prevLogTerm int,
	peers []string,
	majority int,
) bool {
	// Buffered so no goroutine leaks if we return after reaching quorum early.
	results := make(chan int, len(peers))
	for _, peer := range peers {
		go func(peer string) {
			args := &AppendEntriesArgs{
				Term:         currentTerm,
				LeaderID:     rs.nodeID,
				PrevLogIndex: prevLogIndex,
				PrevLogTerm:  prevLogTerm,
				Entries:      nil,
				LeaderCommit: leaderCommit,
			}
			ctx, cancel := context.WithTimeout(context.Background(), appendEntriesTimeout)
			defer cancel()
			reply, err := transport.SendAppendEntries(ctx, peer, args)
			if err != nil {
				results <- -1 // unreachable within the deadline
				return
			}
			results <- reply.Term
		}(peer)
	}

	acks := 1 // count ourselves
	for range peers {
		term := <-results
		switch {
		case term > currentTerm:
			rs.stepDown(term)
			return false
		case term == currentTerm:
			acks++
			if acks >= majority {
				return true
			}
		}
	}
	return false
}

// stepDown reverts this node to a follower at newTerm when newTerm is newer than
// the current term, persisting the change. It self-locks via rs.mu and mirrors
// the higher-term handling used throughout the RPC paths.
func (rs *RaftState) stepDown(newTerm int) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if newTerm > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = newTerm
		rs.persistent.VotedFor = nil
		rs.state = Follower
		rs.currentLeader = ""
		if err := rs.persist(); err != nil {
			rs.logger.Printf("stepDown: persist failed: %v", err)
		}
	}
}
