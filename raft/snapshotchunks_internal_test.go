package raft

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
	"time"
)

// White-box tests for the InstallSnapshot receiver's chunk assembly
// (KNOWN_ISSUES.md R15, paper §7 Figure 13 receiver rules 2-4).
//
// The property under test throughout is that a partial transfer is invisible:
// until the chunk carrying Done arrives, the receiver's Raft state, its state
// machine and both of its files are exactly as they were. That is what keeps the
// durability ordering invariant on RaftState.InstallSnapshot (payload durable →
// boundary durable → in-memory apply, KNOWN_ISSUES.md R3) a single, once-per-
// snapshot sequence rather than something smeared across a transfer that may
// never finish.

// durabilityLog records, in order, the writes a receiver makes to its two
// durable files. Both fakes below append to the same log, so a test can assert
// that the payload reached storage before the Raft boundary — and, for a
// chunked transfer, that neither was touched at all before the final chunk.
type durabilityLog struct {
	mu     sync.Mutex
	writes []string
}

func (d *durabilityLog) record(what string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writes = append(d.writes, what)
}

func (d *durabilityLog) order() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.writes...)
}

// recordingSnapshotter is a Snapshotter that keeps the installed payload in
// memory and logs every write.
type recordingSnapshotter struct {
	mu      sync.Mutex
	log     *durabilityLog
	current *SnapshotData
}

func (r *recordingSnapshotter) CreateSnapshot(int, int) ([]byte, error) { return nil, nil }

func (r *recordingSnapshotter) InstallSnapshot(data []byte, idx, term int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.log.record("payload")
	r.current = &SnapshotData{
		LastIncludedIndex: idx,
		LastIncludedTerm:  term,
		Data:              append([]byte(nil), data...),
	}
	return nil
}

func (r *recordingSnapshotter) ReadSnapshot() (*SnapshotData, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return nil, nil
	}
	envelope := *r.current
	envelope.Data = append([]byte(nil), r.current.Data...)
	return &envelope, nil
}

// installed returns the payload currently "on disk", or nil.
func (r *recordingSnapshotter) installed() *SnapshotData {
	snapshot, _ := r.ReadSnapshot()
	return snapshot
}

// recordingPersister is a Persister that logs every SaveRaftState.
type recordingPersister struct {
	mu    sync.Mutex
	log   *durabilityLog
	saved *PersistentState
}

func (p *recordingPersister) SaveRaftState(state *PersistentState) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.log.record("boundary")
	saved := *state
	p.saved = &saved
	return nil
}

func (p *recordingPersister) LoadRaftState() (*PersistentState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saved == nil {
		return nil, nil
	}
	loaded := *p.saved
	return &loaded, nil
}

func (p *recordingPersister) savedBoundary() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.saved == nil {
		return 0
	}
	return p.saved.LastIncludedIndex
}

// chunkReceiver is a follower wired to a recording snapshotter and persister,
// with a buffered apply channel a test can inspect without draining it.
type chunkReceiver struct {
	rs          *RaftState
	snapshotter *recordingSnapshotter
	persister   *recordingPersister
	durability  *durabilityLog
	applyCh     chan ApplyMsg
}

func newChunkReceiver(t *testing.T, term int) *chunkReceiver {
	t.Helper()

	durability := &durabilityLog{}
	persister := &recordingPersister{log: durability}
	applyCh := make(chan ApplyMsg, 8)

	rs, err := NewRaftStateWithPersister("n1", []string{"n1", "n2", "n3"}, applyCh, persister)
	if err != nil {
		t.Fatalf("create raft state: %v", err)
	}
	t.Cleanup(rs.Stop)

	snapshotter := &recordingSnapshotter{log: durability}
	rs.SetSnapshotter(snapshotter)

	rs.mu.Lock()
	rs.persistent.CurrentTerm = term
	rs.mu.Unlock()

	return &chunkReceiver{
		rs:          rs,
		snapshotter: snapshotter,
		persister:   persister,
		durability:  durability,
		applyCh:     applyCh,
	}
}

// send delivers one chunk and returns the reply.
func (c *chunkReceiver) send(args *InstallSnapshotArgs) *InstallSnapshotReply {
	reply := &InstallSnapshotReply{}
	c.rs.InstallSnapshot(args, reply)
	return reply
}

// assertUntouched checks that nothing about this receiver reflects a snapshot:
// no Raft boundary, no volatile progress, no durable write, nothing queued for
// the state machine. This is the invariant a partial transfer must preserve.
func (c *chunkReceiver) assertUntouched(t *testing.T, when string) {
	t.Helper()

	if idx, term := c.rs.GetSnapshotMetadata(); idx != 0 || term != 0 {
		t.Fatalf("%s: snapshot boundary = (%d, %d), want (0, 0)", when, idx, term)
	}
	if got := c.rs.GetLastApplied(); got != 0 {
		t.Fatalf("%s: LastApplied = %d, want 0", when, got)
	}
	if got := c.rs.GetCommitIndex(); got != 0 {
		t.Fatalf("%s: CommitIndex = %d, want 0", when, got)
	}
	if snapshot := c.snapshotter.installed(); snapshot != nil {
		t.Fatalf("%s: a payload was written to storage before the transfer finished: %+v", when, snapshot)
	}
	if got := c.persister.savedBoundary(); got != 0 {
		t.Fatalf("%s: persisted raft boundary = %d, want 0", when, got)
	}
	// A term change persists the Raft state for its own reasons, so the check
	// here is that no *payload* was written and the boundary on disk has not
	// moved — never that the node wrote nothing at all.
	for _, write := range c.durability.order() {
		if write == "payload" {
			t.Fatalf("%s: a snapshot payload was written before the transfer finished", when)
		}
	}
	select {
	case msg := <-c.applyCh:
		t.Fatalf("%s: snapshot handed to the state machine before the transfer finished: %+v", when, msg)
	default:
	}
}

// chunkArgs builds one chunk of the snapshot at (index, term) for term-`rpcTerm`
// leader "n2".
func chunkArgs(rpcTerm, index, snapTerm, offset int, data []byte, done bool) *InstallSnapshotArgs {
	return &InstallSnapshotArgs{
		Term:              rpcTerm,
		LeaderID:          "n2",
		LastIncludedIndex: index,
		LastIncludedTerm:  snapTerm,
		Offset:            offset,
		Data:              data,
		Done:              done,
	}
}

// splitPayload cuts payload into chunks of at most size bytes, the way the
// leader does.
func splitPayload(payload []byte, size int) [][]byte {
	chunks := make([][]byte, 0, len(payload)/size+1)
	for offset := 0; ; offset += size {
		end := min(offset+size, len(payload))
		chunks = append(chunks, payload[offset:end])
		if end == len(payload) {
			return chunks
		}
	}
}

// bigPayload builds a payload that splits into at least the requested number of
// chunks at the given chunk size, with position-dependent bytes so a
// mis-assembled buffer cannot pass byte comparison.
func bigPayload(chunkSize, chunks int) []byte {
	payload := make([]byte, 0, chunkSize*chunks)
	for i := 0; len(payload) < chunkSize*chunks; i++ {
		payload = append(payload, fmt.Sprintf("entry-%04d;", i)...)
	}
	return payload
}

// TestInstallSnapshotAssemblesChunkedTransfer is the large-snapshot case: a
// payload that takes more than five chunks arrives, and only the last one may
// change anything. The assembled payload must be the leader's byte for byte, and
// the two durable writes must happen once each, payload before boundary.
func TestInstallSnapshotAssemblesChunkedTransfer(t *testing.T) {
	const chunkSize = 64
	receiver := newChunkReceiver(t, 2)

	payload := bigPayload(chunkSize, 6)
	chunks := splitPayload(payload, chunkSize)
	if len(chunks) < 5 {
		t.Fatalf("test needs at least 5 chunks, got %d", len(chunks))
	}

	offset := 0
	for i, chunk := range chunks {
		done := i == len(chunks)-1
		reply := receiver.send(chunkArgs(2, 40, 2, offset, chunk, done))
		if reply.Term != 2 {
			t.Fatalf("chunk %d: reply.Term = %d, want 2", i, reply.Term)
		}
		offset += len(chunk)
		if done {
			break
		}
		receiver.assertUntouched(t, fmt.Sprintf("after chunk %d of %d", i+1, len(chunks)))
		if reply.Offset != offset {
			t.Fatalf("chunk %d: reply.Offset = %d, want %d (the next byte the receiver needs)",
				i, reply.Offset, offset)
		}
	}

	// The final chunk installs the whole payload, exactly once.
	if idx, term := receiver.rs.GetSnapshotMetadata(); idx != 40 || term != 2 {
		t.Fatalf("snapshot boundary = (%d, %d), want (40, 2)", idx, term)
	}
	installed := receiver.snapshotter.installed()
	if installed == nil || !bytes.Equal(installed.Data, payload) {
		t.Fatalf("assembled payload does not match the leader's %d bytes", len(payload))
	}
	if got := receiver.persister.savedBoundary(); got != 40 {
		t.Fatalf("persisted raft boundary = %d, want 40", got)
	}

	// R3's ordering, and nothing extra: one payload write, then one boundary
	// write. A receiver that wrote per chunk would show several of either.
	if got := receiver.durability.order(); len(got) != 2 || got[0] != "payload" || got[1] != "boundary" {
		t.Fatalf("durable writes = %v, want exactly [payload boundary]", got)
	}

	select {
	case msg := <-receiver.applyCh:
		if !msg.SnapshotValid || msg.SnapshotIndex != 40 {
			t.Fatalf("unexpected apply message: %+v", msg)
		}
		if !bytes.Equal(msg.SnapshotData, payload) {
			t.Fatal("the state machine was handed a payload that differs from the leader's")
		}
	case <-time.After(time.Second):
		t.Fatal("the assembled snapshot never reached the state machine")
	}
}

// TestInstallSnapshotInterruptedTransferChangesNothing is the interruption case.
// The leader stops after the third chunk (its RPC failed, it was demoted, it
// crashed). Nothing on the receiver may reflect the half-arrived snapshot, and
// the next round — which starts again from offset 0 — must complete normally.
func TestInstallSnapshotInterruptedTransferChangesNothing(t *testing.T) {
	const chunkSize = 64
	receiver := newChunkReceiver(t, 2)

	payload := bigPayload(chunkSize, 6)
	chunks := splitPayload(payload, chunkSize)

	offset := 0
	for i := 0; i < 3; i++ {
		receiver.send(chunkArgs(2, 40, 2, offset, chunks[i], false))
		offset += len(chunks[i])
	}
	receiver.assertUntouched(t, "after an interrupted transfer")

	// The next replication round reads the snapshot afresh and restarts at
	// offset 0. The abandoned buffer must not contribute a single byte.
	offset = 0
	for i, chunk := range chunks {
		receiver.send(chunkArgs(2, 40, 2, offset, chunk, i == len(chunks)-1))
		offset += len(chunk)
	}

	installed := receiver.snapshotter.installed()
	if installed == nil {
		t.Fatal("the restarted transfer never installed a payload")
	}
	if !bytes.Equal(installed.Data, payload) {
		t.Fatalf("installed payload is %d bytes, want the leader's %d — the abandoned "+
			"buffer leaked into the restarted transfer", len(installed.Data), len(payload))
	}
	if idx, _ := receiver.rs.GetSnapshotMetadata(); idx != 40 {
		t.Fatalf("snapshot boundary = %d after the restarted transfer, want 40", idx)
	}
}

// TestInstallSnapshotHigherTermDiscardsPartialTransfer covers the term change.
// A transfer in progress belongs to one leader in one term; once a newer term
// arrives, its remaining chunks are stale and must not be able to complete an
// install under the old leader's authority.
func TestInstallSnapshotHigherTermDiscardsPartialTransfer(t *testing.T) {
	const chunkSize = 64
	receiver := newChunkReceiver(t, 2)

	payload := bigPayload(chunkSize, 6)
	chunks := splitPayload(payload, chunkSize)

	offset := 0
	for i := 0; i < 3; i++ {
		receiver.send(chunkArgs(2, 40, 2, offset, chunks[i], false))
		offset += len(chunks[i])
	}

	// A new leader takes over in term 5.
	receiver.rs.AppendEntries(
		&AppendEntriesArgs{Term: 5, LeaderID: "n3", PrevLogIndex: 0, PrevLogTerm: 0},
		&AppendEntriesReply{},
	)

	receiver.rs.mu.RLock()
	pending := receiver.rs.pendingChunks
	receiver.rs.mu.RUnlock()
	if pending != nil {
		t.Fatalf("partial transfer from term 2 survived a term change: %d bytes buffered for index %d",
			len(pending.buf), pending.lastIncludedIndex)
	}

	// The deposed leader's remaining chunks are refused on term alone, and the
	// reply tells it to step down.
	for i := 3; i < len(chunks); i++ {
		reply := receiver.send(chunkArgs(2, 40, 2, offset, chunks[i], i == len(chunks)-1))
		if reply.Term != 5 {
			t.Fatalf("chunk %d: reply.Term = %d, want 5", i, reply.Term)
		}
		offset += len(chunks[i])
	}
	receiver.assertUntouched(t, "after stale chunks from the deposed leader")

	// The new leader's own transfer, in term 5, goes through.
	offset = 0
	for i, chunk := range chunks {
		args := chunkArgs(5, 40, 5, offset, chunk, i == len(chunks)-1)
		args.LeaderID = "n3"
		receiver.send(args)
		offset += len(chunk)
	}
	if idx, term := receiver.rs.GetSnapshotMetadata(); idx != 40 || term != 5 {
		t.Fatalf("snapshot boundary = (%d, %d), want (40, 5)", idx, term)
	}
}

// TestInstallSnapshotRejectsOffsetMismatch covers the gap/overlap rule. A chunk
// that does not start exactly where the buffer ends would leave a hole or
// duplicate bytes, so it is refused — and the reply names the offset that fits,
// which is what lets the leader carry on without restarting.
func TestInstallSnapshotRejectsOffsetMismatch(t *testing.T) {
	const chunkSize = 64
	receiver := newChunkReceiver(t, 2)

	payload := bigPayload(chunkSize, 6)
	chunks := splitPayload(payload, chunkSize)

	receiver.send(chunkArgs(2, 40, 2, 0, chunks[0], false))

	// A chunk from further along the payload: accepting it would leave a hole.
	skipped := len(chunks[0]) + len(chunks[1])
	reply := receiver.send(chunkArgs(2, 40, 2, skipped, chunks[2], false))
	if reply.Offset != len(chunks[0]) {
		t.Fatalf("reply.Offset = %d after an out-of-order chunk, want %d (what the receiver holds)",
			reply.Offset, len(chunks[0]))
	}

	// An overlapping chunk is refused for the same reason.
	reply = receiver.send(chunkArgs(2, 40, 2, 0+len(chunks[0])/2, chunks[1], false))
	if reply.Offset != len(chunks[0]) {
		t.Fatalf("reply.Offset = %d after an overlapping chunk, want %d",
			reply.Offset, len(chunks[0]))
	}

	// The buffered prefix survived both refusals, so the leader resumes from the
	// offset it was told and the transfer completes with the correct payload.
	offset := len(chunks[0])
	for i := 1; i < len(chunks); i++ {
		receiver.send(chunkArgs(2, 40, 2, offset, chunks[i], i == len(chunks)-1))
		offset += len(chunks[i])
	}

	installed := receiver.snapshotter.installed()
	if installed == nil || !bytes.Equal(installed.Data, payload) {
		t.Fatal("resuming after a refused chunk did not reassemble the leader's payload")
	}
}

// TestInstallSnapshotChunkForUnknownTransferAsksForRestart covers a non-zero
// offset arriving with nothing buffered — the receiver restarted, or its buffer
// was discarded by a term change. There is nothing to continue, so the reply
// asks for offset 0.
func TestInstallSnapshotChunkForUnknownTransferAsksForRestart(t *testing.T) {
	receiver := newChunkReceiver(t, 2)

	reply := receiver.send(chunkArgs(2, 40, 2, 128, []byte("orphan"), false))
	if reply.Offset != 0 {
		t.Fatalf("reply.Offset = %d for a chunk continuing nothing, want 0 (restart)", reply.Offset)
	}
	receiver.assertUntouched(t, "after an orphaned chunk")
}

// TestInstallSnapshotSingleChunkFormUnchanged is the backward-compatibility
// case: offset 0 with done set is the whole-snapshot message chunking replaced,
// and it must still install in one RPC without buffering anything.
func TestInstallSnapshotSingleChunkFormUnchanged(t *testing.T) {
	receiver := newChunkReceiver(t, 2)

	payload := []byte(`{"kv_data":{"k":"v"}}`)
	reply := receiver.send(chunkArgs(2, 12, 2, 0, payload, true))

	if reply.Term != 2 {
		t.Fatalf("reply.Term = %d, want 2", reply.Term)
	}
	if reply.Offset != 0 {
		t.Fatalf("reply.Offset = %d on a completed single-chunk transfer, want 0", reply.Offset)
	}
	if idx, term := receiver.rs.GetSnapshotMetadata(); idx != 12 || term != 2 {
		t.Fatalf("snapshot boundary = (%d, %d), want (12, 2)", idx, term)
	}
	installed := receiver.snapshotter.installed()
	if installed == nil || !bytes.Equal(installed.Data, payload) {
		t.Fatalf("single-chunk install did not store the payload: %+v", installed)
	}
	if got := receiver.durability.order(); len(got) != 2 || got[0] != "payload" || got[1] != "boundary" {
		t.Fatalf("durable writes = %v, want [payload boundary]", got)
	}

	receiver.rs.mu.RLock()
	defer receiver.rs.mu.RUnlock()
	if receiver.rs.pendingChunks != nil {
		t.Fatal("a single-chunk transfer left an assembly behind")
	}
}

// TestInstallSnapshotEmptyPayloadSingleChunk pins the zero-length snapshot: an
// empty state machine still has a boundary to install, and it travels as one
// chunk with no bytes.
func TestInstallSnapshotEmptyPayloadSingleChunk(t *testing.T) {
	receiver := newChunkReceiver(t, 2)

	receiver.send(chunkArgs(2, 12, 2, 0, nil, true))

	if idx, term := receiver.rs.GetSnapshotMetadata(); idx != 12 || term != 2 {
		t.Fatalf("snapshot boundary = (%d, %d), want (12, 2)", idx, term)
	}
	installed := receiver.snapshotter.installed()
	if installed == nil || len(installed.Data) != 0 {
		t.Fatalf("empty snapshot not installed: %+v", installed)
	}
}

// TestInstallSnapshotChunkResetsElectionTimer pins the invariant that every
// valid InstallSnapshot re-arms the election timer, chunks included. A transfer
// spread over many RPCs is a leader actively talking to this node; treating only
// the final chunk as contact would let a long transfer look like silence and
// trigger an election against a perfectly healthy leader.
func TestInstallSnapshotChunkResetsElectionTimer(t *testing.T) {
	receiver := newChunkReceiver(t, 2)

	before := receiver.rs.GetLastHeartbeat()
	time.Sleep(2 * time.Millisecond)

	// A non-final chunk: nothing is installed, but contact is recorded.
	receiver.send(chunkArgs(2, 40, 2, 0, []byte("first"), false))

	if after := receiver.rs.GetLastHeartbeat(); !after.After(before) {
		t.Fatal("a non-final chunk did not reset the election timer")
	}
	receiver.assertUntouched(t, "after the first chunk")
}
