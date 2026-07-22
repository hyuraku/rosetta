package persistence

import (
	"encoding/json"
	"testing"

	"rosetta/kvstore"
)

// TestRaftSnapshotterReadReturnsV2Bytes verifies that the production
// raft.Snapshotter returns exactly the V2 JSON bytes that KVSnapshotter wrote,
// so a leader shipping InstallSnapshot sends a payload the follower's kvstore
// can parse (KVData + Sessions). Both snapshotters share the same Storage.
func TestRaftSnapshotterReadReturnsV2Bytes(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewFileStorage(dir)
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}

	kvSnap := NewKVSnapshotter(storage)
	sd := &kvstore.SnapshotData{
		KVData: map[string]string{"k": "v"},
		Sessions: map[string]*kvstore.ClientSession{
			"c": {LastSeqNum: 2, LastResult: kvstore.Result{Value: "r"}},
		},
	}
	if err := kvSnap.SaveSnapshotV2(sd, 9, 3); err != nil {
		t.Fatalf("save V2 snapshot: %v", err)
	}

	raftSnap := NewRaftSnapshotter(storage)
	data, err := raftSnap.ReadSnapshot()
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if data == nil {
		t.Fatalf("ReadSnapshot returned nil after a snapshot was saved")
	}

	// The bytes must round-trip back into a V2 SnapshotData with sessions intact.
	var parsed kvstore.SnapshotData
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("ReadSnapshot bytes are not valid V2 JSON: %v", err)
	}
	if parsed.KVData["k"] != "v" {
		t.Fatalf("KVData lost: %+v", parsed.KVData)
	}
	if parsed.Sessions["c"].LastSeqNum != 2 {
		t.Fatalf("Sessions lost: %+v", parsed.Sessions)
	}
}

// TestRaftSnapshotterReadNoSnapshot verifies ReadSnapshot returns (nil, nil)
// when no snapshot exists yet, matching the raft.Snapshotter contract so the
// leader simply skips sending InstallSnapshot.
func TestRaftSnapshotterReadNoSnapshot(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewFileStorage(dir)
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}

	raftSnap := NewRaftSnapshotter(storage)
	data, err := raftSnap.ReadSnapshot()
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	if data != nil {
		t.Fatalf("expected nil bytes when no snapshot exists, got %d bytes", len(data))
	}
}
