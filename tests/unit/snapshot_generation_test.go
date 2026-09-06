package unit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"rosetta/kvstore"
	"rosetta/persistence"
	"rosetta/raft"
)

// These tests pin the durability ordering the InstallSnapshot receive path
// depends on (KNOWN_ISSUES.md R3):
//
//	1. the state machine payload becomes durable,
//	2. the Raft snapshot boundary becomes durable,
//	3. the in-memory state machine is updated.
//
// There is no cross-file atomicity between raft_state.json and snapshot.json, so
// a crash always lands between two steps. This order makes the survivable
// direction the only reachable one: snapshot ahead of Raft, which the startup
// check tolerates and the state machine's monotonicity guard absorbs. The old
// order (boundary first, payload later on the apply path) produced the opposite
// — a node claiming LastApplied = N with its log discarded below N while the
// state machine on disk stopped short of N, and no way to ever deliver the
// missing entries.

// v2Payload builds the V2 snapshot bytes a leader ships.
func v2Payload(t *testing.T, data map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(&kvstore.SnapshotData{KVData: data})
	if err != nil {
		t.Fatalf("marshal snapshot payload: %v", err)
	}
	return raw
}

// newSnapshotReceiver builds a follower whose persister and snapshotter share
// one Storage, exactly as main.go wires them.
func newSnapshotReceiver(t *testing.T, storage *fakeStorage, applyCh chan raft.ApplyMsg) *raft.RaftState {
	t.Helper()
	rs, err := raft.NewRaftStateWithPersister(
		"node1", []string{"node1", "node2"}, applyCh, persistence.NewRaftPersister(storage))
	if err != nil {
		t.Fatalf("create raft state: %v", err)
	}
	rs.SetSnapshotter(persistence.NewRaftSnapshotter(storage))
	return rs
}

func installArgs(index, term int, payload []byte) *raft.InstallSnapshotArgs {
	return &raft.InstallSnapshotArgs{
		Term:              term,
		LeaderID:          "node2",
		LastIncludedIndex: index,
		LastIncludedTerm:  term,
		Data:              payload,
		Done:              true,
	}
}

// TestInstallSnapshotPayloadSaveFailureLeavesRaftUntouched is case (i): the
// state machine payload could not be written, so no Raft state may move. Had the
// boundary been advanced first, this failure would have left the node claiming a
// snapshot it does not have.
func TestInstallSnapshotPayloadSaveFailureLeavesRaftUntouched(t *testing.T) {
	storage := &fakeStorage{}
	applyCh := make(chan raft.ApplyMsg, 4)
	rs := newSnapshotReceiver(t, storage, applyCh)

	storage.failNextSnapshotSaves(1)
	reply := &raft.InstallSnapshotReply{}
	rs.InstallSnapshot(installArgs(7, 2, v2Payload(t, map[string]string{"k": "v"})), reply)

	if reply.Term != 2 {
		t.Fatalf("reply.Term = %d, want 2 (the term is still accepted)", reply.Term)
	}
	if idx, term := rs.GetSnapshotMetadata(); idx != 0 || term != 0 {
		t.Fatalf("snapshot boundary moved to (%d, %d) despite the payload not being saved", idx, term)
	}
	if got := rs.GetLastApplied(); got != 0 {
		t.Fatalf("LastApplied = %d, want 0", got)
	}
	if got := rs.GetCommitIndex(); got != 0 {
		t.Fatalf("CommitIndex = %d, want 0", got)
	}
	if snapshot := storage.savedSnapshot(); snapshot != nil {
		t.Fatalf("snapshot was written despite the injected failure: %+v", snapshot)
	}
	if got := storage.savedState().LastIncludedIndex; got != 0 {
		t.Fatalf("persisted raft boundary = %d, want 0", got)
	}
	select {
	case msg := <-applyCh:
		t.Fatalf("snapshot handed to the state machine after a failed payload save: %+v", msg)
	default:
	}
}

// TestInstallSnapshotBoundaryPersistFailureRollsBack is case (ii): the payload
// is durable but the boundary write failed. Memory must be rolled back so it
// still matches raft_state.json when the handler returns (the R2 discipline),
// leaving only the recoverable "snapshot ahead of Raft" state on disk — and the
// leader's retry must then complete the install.
func TestInstallSnapshotBoundaryPersistFailureRollsBack(t *testing.T) {
	storage := &fakeStorage{}
	// Start at the leader's term so the only SaveRaftState in this handler is
	// the boundary persist we want to fail.
	storage.seedState(raft.PersistentState{CurrentTerm: 2, Log: []raft.LogEntry{}})

	applyCh := make(chan raft.ApplyMsg, 4)
	rs := newSnapshotReceiver(t, storage, applyCh)

	payload := v2Payload(t, map[string]string{"k": "v"})
	storage.failNextStateSaves(1)
	rs.InstallSnapshot(installArgs(7, 2, payload), &raft.InstallSnapshotReply{})

	// Memory rolled back, so memory and disk still agree.
	if idx, term := rs.GetSnapshotMetadata(); idx != 0 || term != 0 {
		t.Fatalf("in-memory boundary = (%d, %d) after a failed persist, want it rolled back to (0, 0)", idx, term)
	}
	if got := rs.GetLastApplied(); got != 0 {
		t.Fatalf("LastApplied = %d after a failed persist, want it rolled back to 0", got)
	}
	if got := rs.GetCommitIndex(); got != 0 {
		t.Fatalf("CommitIndex = %d after a failed persist, want it rolled back to 0", got)
	}
	select {
	case msg := <-applyCh:
		t.Fatalf("snapshot handed to the state machine after a failed boundary persist: %+v", msg)
	default:
	}

	assertPendingCompactionAt(t, storage, 7)
	assertRetryCompletesInstall(t, rs, storage, applyCh, payload)
}

// assertPendingCompactionAt checks that the payload reached disk even though the
// boundary did not, i.e. the crash state is the recoverable "snapshot ahead of
// raft" direction that startup tolerates.
func assertPendingCompactionAt(t *testing.T, storage *fakeStorage, index int) {
	t.Helper()

	snapshot := storage.savedSnapshot()
	if snapshot == nil || snapshot.LastIncludedIndex != index {
		t.Fatalf("payload was not durable before the boundary persist: %+v", snapshot)
	}
	consistency, err := persistence.VerifySnapshotConsistency(storage)
	if err != nil {
		t.Fatalf("crash state after a failed boundary persist is not startable: %v", err)
	}
	if !consistency.CompactionPending {
		t.Fatalf("expected the snapshot to be ahead of the raft state, got %+v", consistency)
	}
}

// assertRetryCompletesInstall drives the leader's retry and checks the install
// finishes, with the state machine told the payload is already durable.
func assertRetryCompletesInstall(
	t *testing.T, rs *raft.RaftState, storage *fakeStorage, applyCh chan raft.ApplyMsg, payload []byte,
) {
	t.Helper()

	rs.InstallSnapshot(installArgs(7, 2, payload), &raft.InstallSnapshotReply{})
	if idx, term := rs.GetSnapshotMetadata(); idx != 7 || term != 2 {
		t.Fatalf("retry did not install the snapshot: boundary = (%d, %d), want (7, 2)", idx, term)
	}
	if got := storage.savedState().LastIncludedIndex; got != 7 {
		t.Fatalf("retry did not persist the boundary: on-disk index = %d, want 7", got)
	}

	select {
	case msg := <-applyCh:
		if !msg.SnapshotValid || msg.SnapshotIndex != 7 {
			t.Fatalf("unexpected apply message: %+v", msg)
		}
		if !msg.SnapshotPersisted {
			t.Fatalf("apply message does not report the payload as already persisted; " +
				"the state machine would write the same generation a second time")
		}
	case <-time.After(time.Second):
		t.Fatal("retry never handed the snapshot to the state machine")
	}
}

// TestInstallSnapshotRestartRecoversOneGeneration is case (iii): after a
// successful install, a restart brings the Raft boundary and the state machine
// up at the same generation.
func TestInstallSnapshotRestartRecoversOneGeneration(t *testing.T) {
	storage := &fakeStorage{}
	kvs := kvstore.NewKVStoreWithSnapshotter(0, persistence.NewKVSnapshotter(storage))
	rs := newSnapshotReceiver(t, storage, kvs.GetApplyCh())

	rs.InstallSnapshot(installArgs(9, 3, v2Payload(t, map[string]string{"k": "v"})), &raft.InstallSnapshotReply{})
	if !waitForKVValue(kvs, "k", "v") {
		t.Fatal("state machine never installed the snapshot")
	}
	kvs.Close()

	// Restart: both halves rebuild from the same Storage.
	consistency, err := persistence.VerifySnapshotConsistency(storage)
	if err != nil {
		t.Fatalf("startup check rejected a cleanly installed snapshot: %v", err)
	}
	if consistency.CompactionPending {
		t.Fatalf("snapshot and raft state are at different generations: %+v", consistency)
	}

	restartedKV := kvstore.NewKVStoreWithSnapshotter(0, persistence.NewKVSnapshotter(storage))
	defer restartedKV.Close()
	restartedRaft, err := raft.NewRaftStateWithPersister(
		"node1", []string{"node1", "node2"}, restartedKV.GetApplyCh(), persistence.NewRaftPersister(storage))
	if err != nil {
		t.Fatalf("restart raft state: %v", err)
	}

	if got := restartedKV.GetSnapshot()["k"]; got != "v" {
		t.Fatalf("restarted state machine lost the snapshot: k = %q, want %q", got, "v")
	}
	if idx, term := restartedRaft.GetSnapshotMetadata(); idx != 9 || term != 3 {
		t.Fatalf("restarted raft boundary = (%d, %d), want (9, 3)", idx, term)
	}
	if got := restartedRaft.GetLastApplied(); got != 9 {
		t.Fatalf("restarted LastApplied = %d, want 9", got)
	}
}

// TestVerifySnapshotConsistencyRefusesRaftAheadOfSnapshot is case (iv). Raft has
// discarded the log below its boundary while the state machine on disk stops
// short of it: nothing can deliver the missing entries again, so startup must
// fail rather than serve incomplete data.
func TestVerifySnapshotConsistencyRefusesRaftAheadOfSnapshot(t *testing.T) {
	storage := &fakeStorage{}
	storage.seedState(raft.PersistentState{CurrentTerm: 4, LastIncludedIndex: 12, LastIncludedTerm: 4})
	storage.seedSnapshot(5, 2, v2Payload(t, map[string]string{"k": "old"}))

	consistency, err := persistence.VerifySnapshotConsistency(storage)
	if err == nil {
		t.Fatalf("startup was allowed with raft at %d and the snapshot at %d",
			consistency.RaftLastIncludedIndex, consistency.SnapshotLastIncludedIndex)
	}
	if !strings.Contains(err.Error(), "R3") {
		t.Fatalf("error does not point at the tracked issue: %v", err)
	}
}

// TestVerifySnapshotConsistencyAbsorbsPendingCompaction is case (v): the
// snapshot is ahead of the Raft state because a compaction did not finish. The
// node starts, Raft replays the committed prefix, and the state machine drops
// every replayed entry it has already applied while still applying new ones.
func TestVerifySnapshotConsistencyAbsorbsPendingCompaction(t *testing.T) {
	storage := &fakeStorage{}
	storage.seedState(raft.PersistentState{CurrentTerm: 4, LastIncludedIndex: 4, LastIncludedTerm: 2})
	storage.seedSnapshot(10, 3, v2Payload(t, map[string]string{"k": "at-10"}))

	consistency, err := persistence.VerifySnapshotConsistency(storage)
	if err != nil {
		t.Fatalf("startup refused a recoverable pending compaction: %v", err)
	}
	if !consistency.CompactionPending {
		t.Fatalf("pending compaction not reported: %+v", consistency)
	}

	kvs := kvstore.NewKVStoreWithSnapshotter(0, persistence.NewKVSnapshotter(storage))
	defer kvs.Close()

	// Replay of an entry the snapshot already covers must not touch the store.
	replay, err := json.Marshal(kvstore.Command{Op: kvstore.OpPut, Key: "k", Value: "replayed", ID: "op-replay"})
	if err != nil {
		t.Fatalf("marshal replayed command: %v", err)
	}
	kvs.GetApplyCh() <- raft.ApplyMsg{CommandValid: true, Command: replay, CommandIndex: 6, CommandTerm: 2}

	// A genuinely new entry still applies, which also proves the replay was
	// consumed rather than blocking the apply loop.
	fresh, err := json.Marshal(kvstore.Command{Op: kvstore.OpPut, Key: "new", Value: "at-11", ID: "op-fresh"})
	if err != nil {
		t.Fatalf("marshal new command: %v", err)
	}
	kvs.GetApplyCh() <- raft.ApplyMsg{CommandValid: true, Command: fresh, CommandIndex: 11, CommandTerm: 4}

	if !waitForKVValue(kvs, "new", "at-11") {
		t.Fatal("entry above the snapshot boundary was never applied")
	}
	if got := kvs.GetSnapshot()["k"]; got != "at-10" {
		t.Fatalf("replayed entry rewrote already-applied state: k = %q, want %q", got, "at-10")
	}
}

// TestTruncateLogToRollsBackOnPersistFailure is case (vi). A compaction whose
// boundary never reached disk must leave the in-memory log and boundary exactly
// as they were, or the node would serve a log it cannot recover after a restart.
func TestTruncateLogToRollsBackOnPersistFailure(t *testing.T) {
	persister := &recordingPersister{}
	applyCh := make(chan raft.ApplyMsg, 8)
	rs, err := raft.NewRaftStateWithPersister("node1", []string{"node1"}, applyCh, persister)
	if err != nil {
		t.Fatalf("create raft state: %v", err)
	}

	const entries = 5
	for i := 0; i < entries; i++ {
		if _, err := rs.AppendLogEntry("cmd", "command"); err != nil {
			t.Fatalf("append entry %d: %v", i, err)
		}
	}

	persister.setFailures(1)
	if err := rs.TruncateLogTo(3); err == nil {
		t.Fatal("TruncateLogTo reported success despite a failed persist")
	}

	if idx, term := rs.GetSnapshotMetadata(); idx != 0 || term != 0 {
		t.Fatalf("boundary advanced to (%d, %d) despite a failed persist", idx, term)
	}
	if got := len(rs.GetLogEntries(1)); got != entries {
		t.Fatalf("log holds %d entries after a failed truncation, want %d", got, entries)
	}
	if got := rs.GetLastLogIndex(); got != entries {
		t.Fatalf("last log index = %d after a failed truncation, want %d", got, entries)
	}

	// Storage recovers and the compaction goes through.
	if err := rs.TruncateLogTo(3); err != nil {
		t.Fatalf("retry of TruncateLogTo failed: %v", err)
	}
	if idx, _ := rs.GetSnapshotMetadata(); idx != 3 {
		t.Fatalf("boundary = %d after a successful truncation, want 3", idx)
	}
	if got := len(persister.savedLog()); got != entries-3 {
		t.Fatalf("persisted log holds %d entries, want %d", got, entries-3)
	}
}
