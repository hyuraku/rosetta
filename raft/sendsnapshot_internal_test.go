package raft

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
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

	t.Cleanup(rs.Stop)

	return rs
}

// relayTransport delivers InstallSnapshot chunks to a real follower RaftState
// and records every one of them, so a test can watch a transfer from both ends.
//
// hook, when set before the transfer starts, runs first and can take an RPC
// over: returning a non-nil reply or an error short-circuits delivery, which is
// how a chunk is made to fail, to answer at a higher term, or to block.
type relayTransport struct {
	mu       sync.Mutex
	follower *RaftState
	sent     []*InstallSnapshotArgs
	hook     func(n int, args *InstallSnapshotArgs) (*InstallSnapshotReply, error)
}

func (r *relayTransport) SendRequestVote(
	ctx context.Context, target string, args *RequestVoteArgs,
) (*RequestVoteReply, error) {
	return &RequestVoteReply{Term: args.Term}, nil
}

func (r *relayTransport) SendAppendEntries(
	ctx context.Context, target string, args *AppendEntriesArgs,
) (*AppendEntriesReply, error) {
	return &AppendEntriesReply{Term: args.Term, Success: true}, nil
}

func (r *relayTransport) SendInstallSnapshot(
	ctx context.Context, target string, args *InstallSnapshotArgs,
) (*InstallSnapshotReply, error) {
	r.mu.Lock()
	captured := *args
	captured.Data = append([]byte(nil), args.Data...)
	r.sent = append(r.sent, &captured)
	n := len(r.sent)
	hook := r.hook
	r.mu.Unlock()

	if hook != nil {
		if reply, err := hook(n, &captured); reply != nil || err != nil {
			return reply, err
		}
	}

	reply := &InstallSnapshotReply{}
	r.follower.InstallSnapshot(args, reply)
	return reply, nil
}

func (r *relayTransport) chunks() []*InstallSnapshotArgs {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*InstallSnapshotArgs(nil), r.sent...)
}

func (r *relayTransport) setHook(hook func(n int, args *InstallSnapshotArgs) (*InstallSnapshotReply, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hook = hook
}

// snapshotTransferFixture is a leader holding a snapshot at (index, term) wired
// to a follower through a relayTransport, with a deliberately tiny chunk size.
type snapshotTransferFixture struct {
	leader    *RaftState
	follower  *chunkReceiver
	transport *relayTransport
	payload   []byte
}

const transferPeerID = "n2"

// newSnapshotTransferFixture builds a leader in term 4 whose snapshot at index
// 40 takes several RPCs to ship, and a follower ready to receive it.
func newSnapshotTransferFixture(t *testing.T) *snapshotTransferFixture {
	t.Helper()

	return newSnapshotTransferFixtureWithPayload(t, bigPayload())
}

func newSnapshotTransferFixtureWithPayload(t *testing.T, payload []byte) *snapshotTransferFixture {
	t.Helper()

	// The follower is a separate RaftState; the leader reaches it through the
	// transport rather than by node id, so their ids need not differ.
	follower := newChunkReceiver(t, 1)

	leader := newLeaderWithBoundary(t, transferPeerID, 4, 40, 4, 5)
	leader.setSnapshotChunkSize(snapshotTestChunkSize)
	leader.SetSnapshotter(&generationSnapshotter{
		current: &SnapshotData{LastIncludedIndex: 40, LastIncludedTerm: 4, Data: payload},
	})

	return &snapshotTransferFixture{
		leader:    leader,
		follower:  follower,
		transport: &relayTransport{follower: follower.rs},
		payload:   payload,
	}
}

// replicate runs one replication round, which falls through to the snapshot
// send path because the follower's nextIndex is below the leader's boundary.
func (f *snapshotTransferFixture) replicate() {
	f.leader.replicateToPeer(f.transport, transferPeerID, 4, 40)
}

func (f *snapshotTransferFixture) matchIndex(t *testing.T) int {
	t.Helper()
	f.leader.mu.RLock()
	defer f.leader.mu.RUnlock()
	if f.leader.leader == nil {
		return 0
	}
	return f.leader.leader.MatchIndex[transferPeerID]
}

// TestSendSnapshotSplitsIntoChunks is the large-snapshot case: a payload larger
// than the chunk size goes out as a contiguous series of RPCs, only the last of
// which is marked done, and the follower ends up holding the leader's bytes.
func TestSendSnapshotSplitsIntoChunks(t *testing.T) {
	fixture := newSnapshotTransferFixture(t)

	fixture.replicate()

	chunks := fixture.transport.chunks()
	if len(chunks) < 5 {
		t.Fatalf("snapshot went out in %d RPCs, want at least 5 for a %d byte payload at chunk size %d",
			len(chunks), len(fixture.payload), snapshotTestChunkSize)
	}

	offset := 0
	for i, chunk := range chunks {
		if chunk.Offset != offset {
			t.Fatalf("chunk %d: Offset = %d, want %d (chunks must be contiguous)", i, chunk.Offset, offset)
		}
		if len(chunk.Data) > snapshotTestChunkSize {
			t.Fatalf("chunk %d carries %d bytes, above the %d byte chunk size",
				i, len(chunk.Data), snapshotTestChunkSize)
		}
		if chunk.LastIncludedIndex != 40 || chunk.LastIncludedTerm != 4 {
			t.Fatalf("chunk %d describes (%d, %d), want every chunk to name the one envelope (40, 4)",
				i, chunk.LastIncludedIndex, chunk.LastIncludedTerm)
		}
		if want := i == len(chunks)-1; chunk.Done != want {
			t.Fatalf("chunk %d: Done = %v, want %v", i, chunk.Done, want)
		}
		offset += len(chunk.Data)
	}
	if offset != len(fixture.payload) {
		t.Fatalf("chunks carried %d bytes in total, want the payload's %d", offset, len(fixture.payload))
	}

	installed := fixture.follower.snapshotter.installed()
	if installed == nil || !bytes.Equal(installed.Data, fixture.payload) {
		t.Fatal("the follower's installed payload differs from the leader's")
	}
	if idx, term := fixture.follower.rs.GetSnapshotMetadata(); idx != 40 || term != 4 {
		t.Fatalf("follower boundary = (%d, %d), want (40, 4)", idx, term)
	}
	if got := fixture.matchIndex(t); got != 40 {
		t.Fatalf("MatchIndex[%s] = %d, want 40", transferPeerID, got)
	}
}

// TestSendSnapshotAbandonsRoundWhenAChunkFails is the interruption case. A chunk
// in the middle of the transfer fails, so the round ends: the follower must be
// exactly as it was, the leader must not credit it with the snapshot, and the
// next round must start over from offset 0 and complete.
func TestSendSnapshotAbandonsRoundWhenAChunkFails(t *testing.T) {
	fixture := newSnapshotTransferFixture(t)

	fixture.transport.setHook(func(n int, args *InstallSnapshotArgs) (*InstallSnapshotReply, error) {
		if n == 3 {
			return nil, errors.New("simulated network failure")
		}
		return nil, nil
	})

	fixture.replicate()

	if got := len(fixture.transport.chunks()); got != 3 {
		t.Fatalf("leader sent %d chunks after the third failed, want it to stop at 3", got)
	}
	fixture.follower.assertUntouched(t, "after an interrupted transfer")
	if got := fixture.matchIndex(t); got != 0 {
		t.Fatalf("MatchIndex[%s] = %d after a failed transfer, want 0", transferPeerID, got)
	}

	// The next tick's round: the network is back and the transfer restarts from
	// the beginning of a freshly read envelope.
	fixture.transport.setHook(nil)
	fixture.replicate()

	retry := fixture.transport.chunks()[3:]
	if len(retry) == 0 || retry[0].Offset != 0 {
		t.Fatalf("the retry did not restart at offset 0: %+v", retry[0])
	}
	installed := fixture.follower.snapshotter.installed()
	if installed == nil || !bytes.Equal(installed.Data, fixture.payload) {
		t.Fatal("the retried transfer did not deliver the leader's payload")
	}
	if got := fixture.matchIndex(t); got != 40 {
		t.Fatalf("MatchIndex[%s] = %d after the retry, want 40", transferPeerID, got)
	}
}

// TestSendSnapshotSendsEmptyPayloadAsOneChunk pins the degenerate case: a
// zero-byte snapshot still describes a boundary the follower must install, so it
// travels as a single empty chunk rather than as no RPC at all.
func TestSendSnapshotSendsEmptyPayloadAsOneChunk(t *testing.T) {
	fixture := newSnapshotTransferFixtureWithPayload(t, nil)

	fixture.replicate()

	chunks := fixture.transport.chunks()
	if len(chunks) != 1 {
		t.Fatalf("an empty snapshot went out in %d RPCs, want exactly 1", len(chunks))
	}
	if chunks[0].Offset != 0 || !chunks[0].Done || len(chunks[0].Data) != 0 {
		t.Fatalf("empty snapshot chunk = %+v, want offset 0, done, no data", chunks[0])
	}
	if idx, term := fixture.follower.rs.GetSnapshotMetadata(); idx != 40 || term != 4 {
		t.Fatalf("follower boundary = (%d, %d), want (40, 4)", idx, term)
	}
	if got := fixture.matchIndex(t); got != 40 {
		t.Fatalf("MatchIndex[%s] = %d, want 40", transferPeerID, got)
	}
}

// TestSendSnapshotStepsDownMidTransfer covers a demotion discovered part-way
// through: the rest of the payload must not be pushed under an authority this
// node no longer holds, and the follower must not be credited with a snapshot it
// never assembled.
func TestSendSnapshotStepsDownMidTransfer(t *testing.T) {
	fixture := newSnapshotTransferFixture(t)

	fixture.transport.setHook(func(n int, args *InstallSnapshotArgs) (*InstallSnapshotReply, error) {
		if n == 2 {
			// A follower that has already moved on to term 9 answers.
			return &InstallSnapshotReply{Term: 9}, nil
		}
		return nil, nil
	})

	fixture.replicate()

	if got := len(fixture.transport.chunks()); got != 2 {
		t.Fatalf("leader sent %d chunks after learning of a newer term, want it to stop at 2", got)
	}
	if state := fixture.leader.GetNodeState(); state != Follower {
		t.Fatalf("node state = %v after a reply at a higher term, want Follower", state)
	}
	if got := fixture.leader.GetCurrentTerm(); got != 9 {
		t.Fatalf("term = %d after stepping down, want 9", got)
	}
	if got := fixture.matchIndex(t); got != 0 {
		t.Fatalf("MatchIndex[%s] = %d after an abandoned transfer, want 0", transferPeerID, got)
	}
	fixture.follower.assertUntouched(t, "after the leader stepped down mid-transfer")
}

// TestSendSnapshotStopsWhenKilled checks the shutdown path: a transfer in
// progress must notice Stop between chunks and let Kill return, rather than
// keeping the WaitGroup busy for the rest of the payload (KNOWN_ISSUES.md R19).
// Run under -race, this also covers the chunk loop reading state the shutdown
// path writes.
func TestSendSnapshotStopsWhenKilled(t *testing.T) {
	fixture := newSnapshotTransferFixture(t)

	firstChunkSent := make(chan struct{})
	release := make(chan struct{})
	fixture.transport.setHook(func(n int, args *InstallSnapshotArgs) (*InstallSnapshotReply, error) {
		if n == 1 {
			close(firstChunkSent)
			<-release
		}
		return nil, nil
	})

	// Spawned the way sendHeartbeats spawns a round, so Stop has to join it.
	if !fixture.leader.spawn(func() { fixture.replicate() }) {
		t.Fatal("could not start the replication round")
	}

	<-firstChunkSent

	stopped := make(chan struct{})
	go func() {
		fixture.leader.Stop()
		close(stopped)
	}()

	// Wait until Stop has actually signaled before letting the first chunk
	// finish, so the loop is guaranteed to see the stop on its next turn.
	<-fixture.leader.stopCh
	close(release)

	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return while a snapshot transfer was in flight")
	}

	if got := len(fixture.transport.chunks()); got != 1 {
		t.Fatalf("leader sent %d chunks after Stop, want it to stop after the one in flight", got)
	}
	fixture.follower.assertUntouched(t, "after the sender was stopped")
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
