package unit

import (
	"encoding/json"
	"testing"
	"time"

	"rosetta/kvstore"
	"rosetta/persistence"
	"rosetta/raft"
)

// TestInstallSnapshotPersistsToDiskAndSurvivesRestart verifies defect 1 & 2:
// when a follower receives an InstallSnapshot (delivered through the apply
// channel) it must persist the snapshot to disk, not only replace it in memory.
// Otherwise a follower that restarts after installing a snapshot would come up
// with neither data nor log. The snapshot payload uses the V2 format (KVData +
// Sessions), and both must be restored after a simulated restart.
func TestInstallSnapshotPersistsToDiskAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	storage, err := persistence.NewFileStorage(dir)
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	snapshotter := persistence.NewKVSnapshotter(storage)

	kvs := kvstore.NewKVStoreWithSnapshotter(0, snapshotter)

	// Build a V2 snapshot payload as a leader's RaftSnapshotter would ship it.
	sd := &kvstore.SnapshotData{
		KVData: map[string]string{"alpha": "1", "beta": "2"},
		Sessions: map[string]*kvstore.ClientSession{
			"client-x": {LastSeqNum: 4, LastResult: kvstore.Result{Value: "cached-4"}},
		},
	}
	raw, err := json.Marshal(sd)
	if err != nil {
		t.Fatalf("marshal snapshot data: %v", err)
	}

	// Deliver the snapshot the same way raft's InstallSnapshot handler does.
	kvs.GetApplyCh() <- raft.ApplyMsg{
		CommandValid:  false,
		SnapshotValid: true,
		SnapshotIndex: 12,
		SnapshotTerm:  4,
		SnapshotData:  raw,
	}

	// Wait for the apply loop to install and (crucially) persist the snapshot.
	if !waitForSnapshotOnDisk(t, storage, 12) {
		t.Fatalf("snapshot was not persisted to disk after InstallSnapshot")
	}
	kvs.Close()

	// Simulate a restart: a fresh KVStore over the same storage must recover
	// both the KV data and the duplicate-detection sessions from disk.
	storage2, err := persistence.NewFileStorage(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	snapshotter2 := persistence.NewKVSnapshotter(storage2)
	kvs2 := kvstore.NewKVStoreWithSnapshotter(0, snapshotter2)
	defer kvs2.Close()

	if got := kvs2.GetSnapshot()["alpha"]; got != "1" {
		t.Fatalf("restart lost KV data: key alpha = %q, want %q", got, "1")
	}
	if got := kvs2.GetSnapshot()["beta"]; got != "2" {
		t.Fatalf("restart lost KV data: key beta = %q, want %q", got, "2")
	}

	// The restored session must still deduplicate a retry with the same seqNum.
	snap, lastIndex, lastTerm, err := snapshotter2.LoadSnapshotV2()
	if err != nil {
		t.Fatalf("load snapshot V2: %v", err)
	}
	if lastIndex != 12 || lastTerm != 4 {
		t.Fatalf("snapshot metadata not persisted: index=%d term=%d, want 12/4", lastIndex, lastTerm)
	}
	sess, ok := snap.Sessions["client-x"]
	if !ok {
		t.Fatalf("session for client-x not persisted in snapshot")
	}
	if sess.LastSeqNum != 4 || sess.LastResult.Value != "cached-4" {
		t.Fatalf("session not restored correctly: %+v", sess)
	}
}

func waitForSnapshotOnDisk(t *testing.T, storage persistence.Storage, wantIndex int) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := storage.LoadSnapshot()
		if err == nil && snap != nil && snap.LastIncludedIndex == wantIndex {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
