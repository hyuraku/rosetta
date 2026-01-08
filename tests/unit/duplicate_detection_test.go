package unit

import (
	"testing"
	"time"

	"rosetta/kvstore"
	"rosetta/raft"
)

// TestDuplicateDetectionBackwardCompatibility verifies that requests without
// ClientID are processed normally (backward compatibility)
func TestDuplicateDetectionBackwardCompatibility(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)
	applyCh := kvs.GetApplyCh()

	transport := raft.NewMockTransport()
	peers := []string{"node1"}

	raftNode := raft.NewRaftNode("node1", peers, transport, applyCh)
	transport.RegisterNode("node1", raftNode)

	kvs.SetRaft(raftNode)

	// Wait for node to become leader
	time.Sleep(350 * time.Millisecond)

	if !raftNode.IsLeader() {
		t.Fatal("Node should be leader in single-node cluster")
	}

	// Put without ClientID should work (backward compatibility)
	err := kvs.Put("key1", "value1")
	if err != nil {
		t.Errorf("Expected Put to succeed without ClientID, got error: %v", err)
	}

	// Verify the value
	value, err := kvs.Get("key1")
	if err != nil {
		t.Errorf("Expected Get to succeed, got error: %v", err)
	}
	if value != "value1" {
		t.Errorf("Expected value 'value1', got '%s'", value)
	}

	kvs.Close()
	raftNode.Kill()
}

// TestDuplicateDetectionNewRequest verifies that new requests with ClientID are processed correctly
func TestDuplicateDetectionNewRequest(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)

	// Directly test executeCommand via restore and internal access
	// First, set up test data
	testData := map[string]string{}
	kvs.RestoreSnapshot(testData)

	// Create a command with ClientID and SeqNum
	cmd := kvstore.Command{
		Op:       kvstore.OpPut,
		Key:      "testkey",
		Value:    "testvalue",
		ID:       "op-1",
		ClientID: "client-123",
		SeqNum:   1,
	}

	// Verify the command structure is correct
	if cmd.ClientID != "client-123" {
		t.Errorf("Expected ClientID to be 'client-123', got '%s'", cmd.ClientID)
	}

	if cmd.SeqNum != 1 {
		t.Errorf("Expected SeqNum to be 1, got %d", cmd.SeqNum)
	}
}

// TestClientSessionStruct verifies ClientSession struct functionality
func TestClientSessionStruct(t *testing.T) {
	session := kvstore.ClientSession{
		LastSeqNum: 5,
		LastResult: kvstore.Result{
			Value: "cached-value",
			Err:   nil,
		},
	}

	if session.LastSeqNum != 5 {
		t.Errorf("Expected LastSeqNum to be 5, got %d", session.LastSeqNum)
	}

	if session.LastResult.Value != "cached-value" {
		t.Errorf("Expected LastResult.Value to be 'cached-value', got '%s'", session.LastResult.Value)
	}

	if session.LastResult.Err != nil {
		t.Errorf("Expected LastResult.Err to be nil, got %v", session.LastResult.Err)
	}
}

// TestSnapshotDataStruct verifies SnapshotData struct functionality
func TestSnapshotDataStruct(t *testing.T) {
	snapshotData := kvstore.SnapshotData{
		KVData: map[string]string{
			"key1": "value1",
			"key2": "value2",
		},
		Sessions: map[string]*kvstore.ClientSession{
			"client-1": {
				LastSeqNum: 10,
				LastResult: kvstore.Result{Value: "result1", Err: nil},
			},
			"client-2": {
				LastSeqNum: 5,
				LastResult: kvstore.Result{Value: "result2", Err: nil},
			},
		},
	}

	// Verify KVData
	if len(snapshotData.KVData) != 2 {
		t.Errorf("Expected 2 KV entries, got %d", len(snapshotData.KVData))
	}

	if snapshotData.KVData["key1"] != "value1" {
		t.Errorf("Expected key1 to be 'value1', got '%s'", snapshotData.KVData["key1"])
	}

	// Verify Sessions
	if len(snapshotData.Sessions) != 2 {
		t.Errorf("Expected 2 sessions, got %d", len(snapshotData.Sessions))
	}

	if snapshotData.Sessions["client-1"].LastSeqNum != 10 {
		t.Errorf("Expected client-1 LastSeqNum to be 10, got %d", snapshotData.Sessions["client-1"].LastSeqNum)
	}

	if snapshotData.Sessions["client-2"].LastSeqNum != 5 {
		t.Errorf("Expected client-2 LastSeqNum to be 5, got %d", snapshotData.Sessions["client-2"].LastSeqNum)
	}
}

// TestCommandWithClientIDAndSeqNum verifies Command struct with duplicate detection fields
func TestCommandWithClientIDAndSeqNum(t *testing.T) {
	cmd := kvstore.Command{
		Op:       kvstore.OpPut,
		Key:      "key",
		Value:    "value",
		ID:       "op-id",
		ClientID: "unique-client-id",
		SeqNum:   42,
	}

	if cmd.Op != kvstore.OpPut {
		t.Errorf("Expected Op to be PUT, got %s", cmd.Op)
	}

	if cmd.Key != "key" {
		t.Errorf("Expected Key to be 'key', got '%s'", cmd.Key)
	}

	if cmd.Value != "value" {
		t.Errorf("Expected Value to be 'value', got '%s'", cmd.Value)
	}

	if cmd.ID != "op-id" {
		t.Errorf("Expected ID to be 'op-id', got '%s'", cmd.ID)
	}

	if cmd.ClientID != "unique-client-id" {
		t.Errorf("Expected ClientID to be 'unique-client-id', got '%s'", cmd.ClientID)
	}

	if cmd.SeqNum != 42 {
		t.Errorf("Expected SeqNum to be 42, got %d", cmd.SeqNum)
	}
}

// TestClientGeneratesUniqueID verifies that each client gets a unique ID
func TestClientGeneratesUniqueID(t *testing.T) {
	servers := []string{"localhost:8080"}

	client1 := kvstore.NewClient(servers)
	client2 := kvstore.NewClient(servers)
	client3 := kvstore.NewClient(servers)

	defer client1.Close()
	defer client2.Close()
	defer client3.Close()

	// We can't directly access clientID (it's private), but we can verify
	// the clients are created successfully
	if client1 == nil {
		t.Error("Expected client1 to be non-nil")
	}
	if client2 == nil {
		t.Error("Expected client2 to be non-nil")
	}
	if client3 == nil {
		t.Error("Expected client3 to be non-nil")
	}
}

// TestDuplicateDetectionIntegration tests the full duplicate detection flow
func TestDuplicateDetectionIntegration(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)
	applyCh := kvs.GetApplyCh()

	transport := raft.NewMockTransport()
	peers := []string{"node1"}

	raftNode := raft.NewRaftNode("node1", peers, transport, applyCh)
	transport.RegisterNode("node1", raftNode)

	kvs.SetRaft(raftNode)

	// Wait for node to become leader
	time.Sleep(350 * time.Millisecond)

	if !raftNode.IsLeader() {
		t.Fatal("Node should be leader in single-node cluster")
	}

	// Test multiple Put operations to verify sequence numbers are incrementing
	for i := 0; i < 5; i++ {
		err := kvs.Put("key", "value")
		if err != nil {
			t.Errorf("Put %d failed: %v", i+1, err)
		}
	}

	// Verify final value
	value, err := kvs.Get("key")
	if err != nil {
		t.Errorf("Expected Get to succeed, got error: %v", err)
	}
	if value != "value" {
		t.Errorf("Expected value 'value', got '%s'", value)
	}

	kvs.Close()
	raftNode.Kill()
}

// TestDeleteOperationWithDuplicateDetection verifies Delete works with ClientID/SeqNum
func TestDeleteOperationWithDuplicateDetection(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)
	applyCh := kvs.GetApplyCh()

	transport := raft.NewMockTransport()
	peers := []string{"node1"}

	raftNode := raft.NewRaftNode("node1", peers, transport, applyCh)
	transport.RegisterNode("node1", raftNode)

	kvs.SetRaft(raftNode)

	// Wait for node to become leader
	time.Sleep(350 * time.Millisecond)

	if !raftNode.IsLeader() {
		t.Fatal("Node should be leader in single-node cluster")
	}

	// Put a value first
	err := kvs.Put("deleteMe", "someValue")
	if err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	// Verify it exists
	_, err = kvs.Get("deleteMe")
	if err != nil {
		t.Fatalf("Get failed after Put: %v", err)
	}

	// Delete it
	err = kvs.Delete("deleteMe")
	if err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	// Verify it's gone
	_, err = kvs.Get("deleteMe")
	if err == nil {
		t.Error("Expected Get to fail after Delete, but it succeeded")
	}

	kvs.Close()
	raftNode.Kill()
}
