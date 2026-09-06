package raft

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

// White-box tests for the leader's InstallSnapshot send path.
//
// The RPC's contract is that (LastIncludedIndex, LastIncludedTerm, Data) describe
// one snapshot. The leader used to sample the index/term from its own
// persistent state under rs.mu.RLock in replicateToPeer and then read the bytes
// from the snapshotter after releasing the lock, so any compaction or snapshot
// install landing in that window produced an RPC that claimed generation A's
// boundary for generation B's payload (KNOWN_ISSUES.md R4). The follower would
// then install a state machine at the wrong log position — a State Machine
// Safety violation that no amount of Raft-level checking can detect.

// captureTransport records the InstallSnapshot RPCs it is asked to send and
// acknowledges them at the sender's own term.
type captureTransport struct {
	mu       sync.Mutex
	snapshot []*InstallSnapshotArgs
}

func (c *captureTransport) SendRequestVote(
	ctx context.Context, target string, args *RequestVoteArgs,
) (*RequestVoteReply, error) {
	return &RequestVoteReply{Term: args.Term}, nil
}

func (c *captureTransport) SendAppendEntries(
	ctx context.Context, target string, args *AppendEntriesArgs,
) (*AppendEntriesReply, error) {
	return &AppendEntriesReply{Term: args.Term, Success: true}, nil
}

func (c *captureTransport) SendInstallSnapshot(
	ctx context.Context, target string, args *InstallSnapshotArgs,
) (*InstallSnapshotReply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	captured := *args
	captured.Data = append([]byte(nil), args.Data...)
	c.snapshot = append(c.snapshot, &captured)
	return &InstallSnapshotReply{Term: args.Term}, nil
}

func (c *captureTransport) sent() []*InstallSnapshotArgs {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*InstallSnapshotArgs(nil), c.snapshot...)
}

// generationSnapshotter serves one snapshot generation at a time and can be told
// to roll over to the next one *during* the ReadSnapshot call — exactly the
// interleaving that used to split metadata from payload.
type generationSnapshotter struct {
	mu      sync.Mutex
	current *SnapshotData
	onRead  func() *SnapshotData
}

func (g *generationSnapshotter) CreateSnapshot(idx, term int) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.current.Data, nil
}

func (g *generationSnapshotter) InstallSnapshot(data []byte, idx, term int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.current = &SnapshotData{LastIncludedIndex: idx, LastIncludedTerm: term, Data: data}
	return nil
}

func (g *generationSnapshotter) ReadSnapshot() (*SnapshotData, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.onRead != nil {
		advance := g.onRead
		g.onRead = nil
		g.current = advance()
	}
	if g.current == nil {
		return nil, nil
	}
	envelope := *g.current
	envelope.Data = append([]byte(nil), g.current.Data...)
	return &envelope, nil
}

// newLeaderWithBoundary builds a leader whose log has already been compacted to
// (lastIncludedIndex, lastIncludedTerm) and which believes peerID needs entries
// from nextIndex onwards.
func newLeaderWithBoundary(
	t *testing.T, peerID string, term, lastIncludedIndex, lastIncludedTerm, nextIndex int,
) *RaftState {
	t.Helper()

	applyCh := make(chan ApplyMsg, 8)
	rs := NewRaftState("n1", []string{"n1", peerID}, applyCh)

	rs.mu.Lock()
	rs.persistent.CurrentTerm = term
	rs.persistent.LastIncludedIndex = lastIncludedIndex
	rs.persistent.LastIncludedTerm = lastIncludedTerm
	rs.persistent.Log = nil
	rs.mu.Unlock()

	rs.SetState(Leader)

	rs.mu.Lock()
	rs.leader.NextIndex[peerID] = nextIndex
	rs.mu.Unlock()

	return rs
}

// TestSendSnapshotUsesOneGeneration is the R4 regression test. The leader
// samples its boundary as (10, 2), and a newer snapshot (14, 4) is published
// before ReadSnapshot returns. The RPC that goes out must describe the snapshot
// whose bytes it carries — (14, 4, "gen-B") — not the stale boundary paired with
// the new payload.
func TestSendSnapshotUsesOneGeneration(t *testing.T) {
	const peerID = "n2"
	rs := newLeaderWithBoundary(t, peerID, 4, 10, 2, 5)

	snapshotter := &generationSnapshotter{
		current: &SnapshotData{LastIncludedIndex: 10, LastIncludedTerm: 2, Data: []byte("gen-A")},
		onRead: func() *SnapshotData {
			// A compaction (or an InstallSnapshot from a newer leader) publishes
			// generation B after the boundary was sampled under rs.mu.
			return &SnapshotData{LastIncludedIndex: 14, LastIncludedTerm: 4, Data: []byte("gen-B")}
		},
	}
	rs.SetSnapshotter(snapshotter)

	transport := &captureTransport{}
	rs.replicateToPeer(transport, peerID, 4, 10)

	sent := transport.sent()
	if len(sent) != 1 {
		t.Fatalf("expected exactly one InstallSnapshot RPC, got %d", len(sent))
	}
	args := sent[0]
	if !bytes.Equal(args.Data, []byte("gen-B")) {
		t.Fatalf("payload = %q, want the generation the envelope was read from (%q)", args.Data, "gen-B")
	}
	if args.LastIncludedIndex != 14 || args.LastIncludedTerm != 4 {
		t.Fatalf("metadata = (%d, %d) with payload %q: metadata and payload come from different generations; want (14, 4)",
			args.LastIncludedIndex, args.LastIncludedTerm, args.Data)
	}

	// The follower installed generation B, so its match index follows that
	// envelope's boundary rather than the boundary we sampled before sending.
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	if got := rs.leader.MatchIndex[peerID]; got != 14 {
		t.Fatalf("MatchIndex[%s] = %d, want 14 (the boundary actually shipped)", peerID, got)
	}
	if got := rs.leader.NextIndex[peerID]; got != 15 {
		t.Fatalf("NextIndex[%s] = %d, want 15", peerID, got)
	}
}

// TestSendSnapshotSkipsStaleEnvelope covers the other half of decoupling the
// boundary from the payload: if the only snapshot we can read ends before the
// entry the follower already has, shipping it would drag that follower's match
// index backwards. Nothing is sent and the follower's indices are untouched.
func TestSendSnapshotSkipsStaleEnvelope(t *testing.T) {
	const peerID = "n2"
	// The leader compacted to 30 but its snapshot file is still at 15, while the
	// follower already holds through index 19.
	rs := newLeaderWithBoundary(t, peerID, 4, 30, 3, 20)

	snapshotter := &generationSnapshotter{
		current: &SnapshotData{LastIncludedIndex: 15, LastIncludedTerm: 2, Data: []byte("stale")},
	}
	rs.SetSnapshotter(snapshotter)

	transport := &captureTransport{}
	rs.replicateToPeer(transport, peerID, 4, 30)

	if sent := transport.sent(); len(sent) != 0 {
		t.Fatalf("sent a snapshot ending at index %d to a follower whose nextIndex is 20",
			sent[0].LastIncludedIndex)
	}

	rs.mu.RLock()
	defer rs.mu.RUnlock()
	if got := rs.leader.NextIndex[peerID]; got != 20 {
		t.Fatalf("NextIndex[%s] = %d, want it left at 20", peerID, got)
	}
	if got := rs.leader.MatchIndex[peerID]; got != 0 {
		t.Fatalf("MatchIndex[%s] = %d, want it left at 0", peerID, got)
	}
}
