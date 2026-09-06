package raft

import (
	"context"
	"sync"
	"testing"
	"time"
)

// White-box tests for per-peer replication serialization.
//
// sendHeartbeats used to spawn a goroutine per peer on every 50ms tick with
// nothing stopping the previous round, so one follower could have a stream of
// AppendEntries and a 5-second InstallSnapshot outstanding at once. Their
// replies then landed in whatever order they finished in, and the success path
// assigned MatchIndex = prevLogIndex + len(entries) unconditionally: a slow,
// smaller round answered last rewound the follower's MatchIndex and NextIndex,
// re-sending entries it had already acknowledged.

// reorderTransport parks the first AppendEntries until release is closed, so a
// later round can be answered before it — the reply reordering the leader has to
// tolerate.
type reorderTransport struct {
	firstOnce    sync.Once
	firstStarted chan struct{}
	release      chan struct{}
}

func newReorderTransport() *reorderTransport {
	return &reorderTransport{
		firstStarted: make(chan struct{}),
		release:      make(chan struct{}),
	}
}

func (r *reorderTransport) SendRequestVote(
	_ context.Context, _ string, args *RequestVoteArgs,
) (*RequestVoteReply, error) {
	return &RequestVoteReply{Term: args.Term}, nil
}

func (r *reorderTransport) SendAppendEntries(
	_ context.Context, _ string, args *AppendEntriesArgs,
) (*AppendEntriesReply, error) {
	first := false
	r.firstOnce.Do(func() { first = true })
	if first {
		close(r.firstStarted)
		<-r.release
	}
	return &AppendEntriesReply{Term: args.Term, Success: true}, nil
}

func (r *reorderTransport) SendInstallSnapshot(
	_ context.Context, _ string, args *InstallSnapshotArgs,
) (*InstallSnapshotReply, error) {
	return &InstallSnapshotReply{Term: args.Term}, nil
}

// newLeaderWithLog builds a leader at the given term whose log holds entryCount
// current-term entries and which believes peerID needs everything from index 1.
func newLeaderWithLog(t *testing.T, peerID string, term, entryCount int) (*RaftState, func()) {
	t.Helper()

	// The drainer stops on its own channel and never closes applyCh: a
	// replication goroutine can still be inside applyEntries when the test
	// finishes, and closing under it is the very panic KNOWN_ISSUES.md R19
	// describes.
	applyCh := make(chan ApplyMsg, 256)
	stopDrain := make(chan struct{})
	go func() {
		for {
			select {
			case <-applyCh:
			case <-stopDrain:
				return
			}
		}
	}()

	rs := NewRaftState("n1", []string{"n1", peerID}, applyCh)
	rs.mu.Lock()
	rs.persistent.CurrentTerm = term
	rs.mu.Unlock()
	rs.SetState(Leader)

	for i := 0; i < entryCount; i++ {
		if _, err := rs.AppendLogEntry("cmd", entryTypeCommand); err != nil {
			t.Fatalf("seeding the log: %v", err)
		}
	}
	rs.mu.Lock()
	rs.leader.NextIndex[peerID] = 1
	rs.mu.Unlock()

	return rs, func() { close(stopDrain) }
}

func (rs *RaftState) peerProgress(peerID string) (matchIndex, nextIndex, commitIndex int) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.leader.MatchIndex[peerID], rs.leader.NextIndex[peerID], rs.volatile.CommitIndex
}

// TestOutOfOrderAppendRepliesDoNotRewindPeer sends a round covering three
// entries, holds its reply, lets a later round covering five entries complete,
// and only then releases the first. The stale ACK must not pull the follower's
// MatchIndex/NextIndex — or the commit index they feed — backwards.
func TestOutOfOrderAppendRepliesDoNotRewindPeer(t *testing.T) {
	const (
		peerID = "n2"
		term   = 1
	)
	rs, stop := newLeaderWithLog(t, peerID, term, 3)
	defer stop()

	transport := newReorderTransport()

	stale := make(chan struct{})
	go func() {
		defer close(stale)
		// Covers entries 1..3; its reply is parked inside the transport.
		rs.replicateToPeer(transport, peerID, term, 0)
	}()
	<-transport.firstStarted

	// Two more entries commit while the first round is still outstanding, and a
	// second round covering 1..5 is answered immediately.
	for i := 0; i < 2; i++ {
		if _, err := rs.AppendLogEntry("cmd", entryTypeCommand); err != nil {
			t.Fatalf("AppendLogEntry: %v", err)
		}
	}
	rs.mu.Lock()
	rs.leader.NextIndex[peerID] = 1
	rs.mu.Unlock()
	rs.replicateToPeer(transport, peerID, term, 0)

	matchIndex, nextIndex, commitIndex := rs.peerProgress(peerID)
	if matchIndex != 5 || nextIndex != 6 {
		t.Fatalf("after the newer reply: MatchIndex=%d NextIndex=%d, want 5 and 6", matchIndex, nextIndex)
	}
	if commitIndex != 5 {
		t.Fatalf("after the newer reply: CommitIndex=%d, want 5", commitIndex)
	}

	// Now let the older, smaller round answer.
	close(transport.release)
	<-stale

	matchIndex, nextIndex, commitIndex = rs.peerProgress(peerID)
	if matchIndex != 5 {
		t.Fatalf("MatchIndex=%d after a reply for entries 1..3 arrived late; it must stay at the "+
			"high-water mark 5 — the follower cannot un-acknowledge entries", matchIndex)
	}
	if nextIndex != 6 {
		t.Fatalf("NextIndex=%d after the late reply, want 6; rewinding it re-sends acknowledged entries", nextIndex)
	}
	if commitIndex != 5 {
		t.Fatalf("CommitIndex=%d after the late reply, want 5", commitIndex)
	}
}

// gatedTransport blocks every AppendEntries until release is closed and records
// how many were ever concurrently in flight per target.
type gatedTransport struct {
	mu          sync.Mutex
	inFlight    map[string]int
	maxInFlight map[string]int
	total       int

	startedOnce sync.Once
	started     chan struct{}
	release     chan struct{}
}

func newGatedTransport() *gatedTransport {
	return &gatedTransport{
		inFlight:    make(map[string]int),
		maxInFlight: make(map[string]int),
		started:     make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (g *gatedTransport) SendRequestVote(
	_ context.Context, _ string, args *RequestVoteArgs,
) (*RequestVoteReply, error) {
	return &RequestVoteReply{Term: args.Term}, nil
}

func (g *gatedTransport) SendAppendEntries(
	_ context.Context, target string, args *AppendEntriesArgs,
) (*AppendEntriesReply, error) {
	g.mu.Lock()
	g.inFlight[target]++
	g.total++
	if g.inFlight[target] > g.maxInFlight[target] {
		g.maxInFlight[target] = g.inFlight[target]
	}
	g.mu.Unlock()

	g.startedOnce.Do(func() { close(g.started) })
	<-g.release

	g.mu.Lock()
	g.inFlight[target]--
	g.mu.Unlock()
	return &AppendEntriesReply{Term: args.Term, Success: true}, nil
}

func (g *gatedTransport) SendInstallSnapshot(
	_ context.Context, _ string, args *InstallSnapshotArgs,
) (*InstallSnapshotReply, error) {
	return &InstallSnapshotReply{Term: args.Term}, nil
}

func (g *gatedTransport) peak(target string) (maxInFlight, current, total int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxInFlight[target], g.inFlight[target], g.total
}

// TestHeartbeatsSerializePerPeer drives many ticks against a follower that never
// answers. Only one AppendEntries may be outstanding to it at a time; the other
// ticks must skip the peer instead of stacking another RPC on top.
func TestHeartbeatsSerializePerPeer(t *testing.T) {
	const (
		peerID = "n2"
		term   = 1
		ticks  = 20
	)
	rs, stop := newLeaderWithLog(t, peerID, term, 1)
	defer stop()

	transport := newGatedTransport()
	for i := 0; i < ticks; i++ {
		rs.sendHeartbeats(transport)
	}
	<-transport.started

	// Give any extra rounds the time they would need to pile up. With the
	// serialization in place there is nothing to wait for; without it the other
	// nineteen goroutines reach the transport well inside this window.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if maxInFlight, _, _ := transport.peak(peerID); maxInFlight > 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	maxInFlight, _, total := transport.peak(peerID)
	if maxInFlight != 1 {
		t.Fatalf("%d AppendEntries were in flight to %s at once (want 1): %d ticks each spawned "+
			"a round with nothing checking whether the previous one had finished", maxInFlight, peerID, ticks)
	}
	if total != 1 {
		t.Fatalf("%d AppendEntries RPCs were sent to %s for %d ticks, want 1 while the first is "+
			"still outstanding", total, peerID, ticks)
	}

	close(transport.release)
	drainDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, current, _ := transport.peak(peerID); current == 0 {
			break
		}
		if time.Now().After(drainDeadline) {
			t.Fatal("replication rounds did not finish after the transport was released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
