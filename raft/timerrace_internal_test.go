package raft

import (
	"sync"
	"testing"
)

// Race regression test for the election timer (KNOWN_ISSUES.md E1).
//
// ResetElectionTimer used to take no lock at all while writing electionTimeout,
// electionTimer and lastHeartbeat. startElection called it after releasing
// rs.mu, the RPC handlers called it while holding rs.mu, so the two sides wrote
// the same fields with no mutual exclusion. Run this with -race: before the fix
// it reports a data race on RaftState.electionTimeout / lastHeartbeat.

// raceIterations is high enough to interleave the two paths reliably and low
// enough to stay fast under -race. The test is iteration-bound, not time-bound,
// so it does not depend on scheduling luck to terminate.
const raceIterations = 500

func TestElectionTimerResetsAreSerialized(t *testing.T) {
	applyCh := make(chan ApplyMsg, 8)
	go func() {
		for range applyCh {
		}
	}()

	// Three peers with an unreachable transport: every election times out
	// without a quorum, so the node loops as a candidate and never becomes a
	// leader (which would stop the timer instead of resetting it).
	rs := NewRaftState("n1", []string{"n1", "n2", "n3"}, applyCh)

	var wg sync.WaitGroup
	wg.Add(2)

	// The candidate side: startElection re-arms the timer for the election it
	// just started.
	go func() {
		defer wg.Done()
		for i := 0; i < raceIterations; i++ {
			rs.startElection(unreachablePeers{})
		}
	}()

	// The receive side: every valid AppendEntries resets the timer under rs.mu.
	// The term is sampled from the node itself so the request is never rejected
	// as stale while the other goroutine keeps incrementing it.
	go func() {
		defer wg.Done()
		for i := 0; i < raceIterations; i++ {
			args := &AppendEntriesArgs{
				Term:         rs.GetCurrentTerm(),
				LeaderID:     "n2",
				PrevLogIndex: 0,
				PrevLogTerm:  0,
				LeaderCommit: 0,
			}
			rs.AppendEntries(args, &AppendEntriesReply{})
		}
	}()

	wg.Wait()
}
