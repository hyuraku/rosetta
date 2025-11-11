package integration

import (
	"testing"
	"time"

	"rosetta/kvstore"
	"rosetta/persistence"
	"rosetta/raft"
)

func TestRaftPersistence_CrashRecovery(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage and persister
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	persister := persistence.NewRaftPersister(storage)
	applyCh := make(chan raft.ApplyMsg, 10)

	// Create first Raft instance
	peers := []string{"node1", "node2", "node3"}
	transport1 := raft.NewMockTransport()

	raftNode1 := raft.NewRaftNodeWithPersister("node1", peers, transport1, applyCh, persister)
	transport1.RegisterNode("node1", raftNode1)

	// Simulate some state changes
	raftNode1.GetRaftState().IncrementTerm()
	raftNode1.GetRaftState().IncrementTerm()
	raftNode1.GetRaftState().IncrementTerm()

	nodeID := "node2"
	raftNode1.GetRaftState().SetVotedFor(&nodeID)

	// Add some log entries
	raftNode1.GetRaftState().AppendLogEntry("cmd1", "command")
	raftNode1.GetRaftState().AppendLogEntry("cmd2", "command")
	raftNode1.GetRaftState().AppendLogEntry("cmd3", "command")

	// Wait a bit for persistence to complete
	time.Sleep(100 * time.Millisecond)

	// Get current state before "crash"
	term1, _ := raftNode1.GetState()
	votedFor1 := raftNode1.GetRaftState().GetVotedFor()
	logLen1 := raftNode1.GetLogLength()

	// Simulate crash by killing the node
	raftNode1.Kill()
	time.Sleep(50 * time.Millisecond)

	// Create new Raft instance with same persister (simulating recovery)
	applyCh2 := make(chan raft.ApplyMsg, 10)
	transport2 := raft.NewMockTransport()

	raftNode2 := raft.NewRaftNodeWithPersister("node1", peers, transport2, applyCh2, persister)
	transport2.RegisterNode("node1", raftNode2)

	// Verify recovered state
	term2, _ := raftNode2.GetState()
	votedFor2 := raftNode2.GetRaftState().GetVotedFor()
	logLen2 := raftNode2.GetLogLength()

	if term2 != term1 {
		t.Errorf("Term not recovered: got %d, want %d", term2, term1)
	}

	if votedFor2 == nil || votedFor1 == nil || *votedFor2 != *votedFor1 {
		t.Errorf("VotedFor not recovered: got %v, want %v", votedFor2, votedFor1)
	}

	if logLen2 != logLen1 {
		t.Errorf("Log length not recovered: got %d, want %d", logLen2, logLen1)
	}

	// Cleanup
	raftNode2.Kill()
	storage.Close()
}

func TestKVStorePersistence_SnapshotRecovery(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage and snapshotter
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	snapshotter := persistence.NewKVSnapshotter(storage)

	// Create first KV store instance
	kvStore1 := kvstore.NewKVStoreWithSnapshotter(1000, snapshotter)

	// Manually set some data (simulating applied operations)
	kvStore1.RestoreSnapshot(map[string]string{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	})

	// Save snapshot
	if err := snapshotter.SaveSnapshot(kvStore1.GetSnapshot(), 10, 5); err != nil {
		t.Fatalf("Failed to save snapshot: %v", err)
	}

	// Get size before "crash"
	size1 := kvStore1.Size()

	// Simulate crash
	kvStore1.Close()
	time.Sleep(50 * time.Millisecond)

	// Create new KV store instance with same snapshotter (simulating recovery)
	kvStore2 := kvstore.NewKVStoreWithSnapshotter(1000, snapshotter)

	// Verify recovered data
	size2 := kvStore2.Size()

	if size2 != size1 {
		t.Errorf("Size not recovered: got %d, want %d", size2, size1)
	}

	// Verify individual keys
	snapshot := kvStore2.GetSnapshot()

	expectedKeys := map[string]string{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	for key, expectedValue := range expectedKeys {
		if value, exists := snapshot[key]; !exists {
			t.Errorf("Key %s not found after recovery", key)
		} else if value != expectedValue {
			t.Errorf("Key %s value mismatch: got %s, want %s", key, value, expectedValue)
		}
	}

	// Cleanup
	kvStore2.Close()
	storage.Close()
}

func TestFullSystemPersistence_CrashAndRecover(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	persister := persistence.NewRaftPersister(storage)
	snapshotter := persistence.NewKVSnapshotter(storage)

	// Create KV store with snapshotter
	kvStore1 := kvstore.NewKVStoreWithSnapshotter(1000, snapshotter)
	applyCh1 := kvStore1.GetApplyCh()

	// Create Raft node with persister
	peers := []string{"node1"}
	transport1 := raft.NewMockTransport()
	raftNode1 := raft.NewRaftNodeWithPersister("node1", peers, transport1, applyCh1, persister)
	transport1.RegisterNode("node1", raftNode1)

	kvStore1.SetRaft(raftNode1)

	// Wait for node to stabilize
	time.Sleep(200 * time.Millisecond)

	// Perform some operations
	testData := map[string]string{
		"testkey1": "testvalue1",
		"testkey2": "testvalue2",
		"testkey3": "testvalue3",
	}

	for key, value := range testData {
		if err := kvStore1.Put(key, value); err != nil {
			t.Fatalf("Failed to put key %s: %v", key, err)
		}
	}

	// Wait for operations to be applied
	time.Sleep(500 * time.Millisecond)

	// Save snapshot
	snapshot1 := kvStore1.GetSnapshot()
	if err := snapshotter.SaveSnapshot(snapshot1, 10, 1); err != nil {
		t.Fatalf("Failed to save snapshot: %v", err)
	}

	// Verify data before crash
	for key, expectedValue := range testData {
		value, err := kvStore1.Get(key)
		if err != nil {
			t.Errorf("Failed to get key %s before crash: %v", key, err)
			continue
		}
		if value != expectedValue {
			t.Errorf("Key %s value mismatch before crash: got %s, want %s", key, value, expectedValue)
		}
	}

	// Get state before crash
	term1, _ := raftNode1.GetState()
	logLen1 := raftNode1.GetLogLength()

	// Simulate crash
	raftNode1.Kill()
	kvStore1.Close()
	time.Sleep(100 * time.Millisecond)

	// Create new instances (simulating recovery)
	kvStore2 := kvstore.NewKVStoreWithSnapshotter(1000, snapshotter)
	applyCh2 := kvStore2.GetApplyCh()

	transport2 := raft.NewMockTransport()
	raftNode2 := raft.NewRaftNodeWithPersister("node1", peers, transport2, applyCh2, persister)
	transport2.RegisterNode("node1", raftNode2)

	kvStore2.SetRaft(raftNode2)

	// Wait for recovery
	time.Sleep(200 * time.Millisecond)

	// Verify Raft state recovered
	term2, _ := raftNode2.GetState()
	logLen2 := raftNode2.GetLogLength()

	if term2 < term1 {
		t.Errorf("Term went backwards after recovery: got %d, want >= %d", term2, term1)
	}

	if logLen2 != logLen1 {
		t.Logf("Log length changed after recovery: got %d, was %d (this may be expected)", logLen2, logLen1)
	}

	// Verify KV data recovered
	snapshot2 := kvStore2.GetSnapshot()

	if len(snapshot2) != len(testData) {
		t.Errorf("Snapshot size mismatch after recovery: got %d, want %d", len(snapshot2), len(testData))
	}

	for key, expectedValue := range testData {
		if value, exists := snapshot2[key]; !exists {
			t.Errorf("Key %s not found after recovery", key)
		} else if value != expectedValue {
			t.Errorf("Key %s value mismatch after recovery: got %s, want %s", key, value, expectedValue)
		}
	}

	// Cleanup
	raftNode2.Kill()
	kvStore2.Close()
	storage.Close()
}

func TestPersistence_MultipleCrashes(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer storage.Close()

	persister := persistence.NewRaftPersister(storage)

	peers := []string{"node1"}

	// Simulate multiple crash-recovery cycles
	for cycle := 1; cycle <= 5; cycle++ {
		t.Logf("Crash-recovery cycle %d", cycle)

		applyCh := make(chan raft.ApplyMsg, 10)
		transport := raft.NewMockTransport()
		raftNode := raft.NewRaftNodeWithPersister("node1", peers, transport, applyCh, persister)
		transport.RegisterNode("node1", raftNode)

		// Wait for recovery
		time.Sleep(100 * time.Millisecond)

		// Verify term increases or stays the same across cycles
		term, _ := raftNode.GetState()
		if term < cycle-1 {
			t.Errorf("Cycle %d: term went backwards: got %d, want >= %d", cycle, term, cycle-1)
		}

		// Make some state changes
		raftNode.GetRaftState().IncrementTerm()
		raftNode.GetRaftState().AppendLogEntry("cmd", "command")

		// Wait for persistence
		time.Sleep(50 * time.Millisecond)

		// Simulate crash
		raftNode.Kill()
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("Successfully completed %d crash-recovery cycles", 5)
}
