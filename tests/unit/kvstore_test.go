package unit

import (
	"testing"
	"time"

	"rosetta/kvstore"
	"rosetta/raft"
)

func TestKVStoreInitialization(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)

	if kvs == nil {
		t.Fatal("NewKVStore returned nil")
	}

	if kvs.Size() != 0 {
		t.Errorf("Expected initial size to be 0, got %d", kvs.Size())
	}

	keys := kvs.Keys()
	if len(keys) != 0 {
		t.Errorf("Expected no keys initially, got %d", len(keys))
	}
}

func TestKVStoreSnapshot(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)

	testData := map[string]string{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	kvs.RestoreSnapshot(testData)

	if kvs.Size() != 3 {
		t.Errorf("Expected size to be 3 after restore, got %d", kvs.Size())
	}

	snapshot := kvs.GetSnapshot()
	if len(snapshot) != 3 {
		t.Errorf("Expected snapshot size to be 3, got %d", len(snapshot))
	}

	for key, expectedValue := range testData {
		if actualValue, exists := snapshot[key]; !exists {
			t.Errorf("Expected key %s to exist in snapshot", key)
		} else if actualValue != expectedValue {
			t.Errorf("Expected value %s for key %s, got %s", expectedValue, key, actualValue)
		}
	}
}

func TestKVStoreClear(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)

	testData := map[string]string{
		"key1": "value1",
		"key2": "value2",
	}

	kvs.RestoreSnapshot(testData)

	if kvs.Size() != 2 {
		t.Errorf("Expected size to be 2 before clear, got %d", kvs.Size())
	}

	kvs.Clear()

	if kvs.Size() != 0 {
		t.Errorf("Expected size to be 0 after clear, got %d", kvs.Size())
	}

	keys := kvs.Keys()
	if len(keys) != 0 {
		t.Errorf("Expected no keys after clear, got %d", len(keys))
	}
}

func TestKVStoreWithMockRaft(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	kvs := kvstore.NewKVStore(1000)

	transport := raft.NewMockTransport()
	peers := []string{"node1"}

	raftNode := raft.NewRaftNode("node1", peers, transport, applyCh)
	transport.RegisterNode("node1", raftNode)

	kvs.SetRaft(raftNode)

	time.Sleep(100 * time.Millisecond)

	raftNode.GetRaftState().SetState(raft.Leader)

	err := kvs.Put("testkey", "testvalue")

	if err != nil {
		t.Errorf("Expected Put to succeed, got error: %v", err)
	}

	raftNode.Kill()
}

func TestCommandSerialization(t *testing.T) {
	cmd := kvstore.Command{
		Op:    kvstore.OpPut,
		Key:   "testkey",
		Value: "testvalue",
		ID:    "test-id-123",
	}

	if cmd.Op != kvstore.OpPut {
		t.Errorf("Expected operation to be PUT, got %s", cmd.Op)
	}

	if cmd.Key != "testkey" {
		t.Errorf("Expected key to be 'testkey', got %s", cmd.Key)
	}

	if cmd.Value != "testvalue" {
		t.Errorf("Expected value to be 'testvalue', got %s", cmd.Value)
	}

	if cmd.ID != "test-id-123" {
		t.Errorf("Expected ID to be 'test-id-123', got %s", cmd.ID)
	}
}

func TestOperationTypes(t *testing.T) {
	if kvstore.OpPut != "PUT" {
		t.Errorf("Expected OpPut to be 'PUT', got %s", kvstore.OpPut)
	}

	if kvstore.OpGet != "GET" {
		t.Errorf("Expected OpGet to be 'GET', got %s", kvstore.OpGet)
	}

	if kvstore.OpDelete != "DELETE" {
		t.Errorf("Expected OpDelete to be 'DELETE', got %s", kvstore.OpDelete)
	}
}

func TestResultStruct(t *testing.T) {
	result := kvstore.Result{
		Value: "test value",
		Err:   nil,
	}

	if result.Value != "test value" {
		t.Errorf("Expected value to be 'test value', got %s", result.Value)
	}

	if result.Err != nil {
		t.Errorf("Expected error to be nil, got %v", result.Err)
	}
}

func TestKVStoreApplyCh(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)

	applyCh := kvs.GetApplyCh()
	if applyCh == nil {
		t.Fatal("Expected apply channel to be non-nil")
	}

	select {
	case applyCh <- raft.ApplyMsg{CommandValid: false}:
	default:
		t.Error("Expected to be able to send to apply channel")
	}
}

func TestKVStoreKeys(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)

	testData := map[string]string{
		"alpha": "1",
		"beta":  "2",
		"gamma": "3",
	}

	kvs.RestoreSnapshot(testData)

	keys := kvs.Keys()
	if len(keys) != 3 {
		t.Errorf("Expected 3 keys, got %d", len(keys))
	}

	keySet := make(map[string]bool)
	for _, key := range keys {
		keySet[key] = true
	}

	for expectedKey := range testData {
		if !keySet[expectedKey] {
			t.Errorf("Expected key %s to be in keys list", expectedKey)
		}
	}
}

func TestKVStoreClose(t *testing.T) {
	kvs := kvstore.NewKVStore(1000)

	applyCh := kvs.GetApplyCh()

	kvs.Close()

	select {
	case _, ok := <-applyCh:
		if ok {
			t.Error("Expected apply channel to be closed")
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Expected apply channel to be closed within timeout")
	}
}
