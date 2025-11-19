package unit

import (
	"testing"
	"time"

	"rosetta/raft"
)

func TestSnapshotMetadata(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1"}

	rs := raft.NewRaftState("node1", peers, applyCh)

	// Initially, snapshot metadata should be zero
	lastIndex, lastTerm := rs.GetSnapshotMetadata()
	if lastIndex != 0 || lastTerm != 0 {
		t.Errorf("Initial snapshot metadata should be zero: got index=%d, term=%d", lastIndex, lastTerm)
	}
}

func TestShouldTakeSnapshot(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1"}

	rs := raft.NewRaftState("node1", peers, applyCh)

	// Initially should not need snapshot
	if rs.ShouldTakeSnapshot(100) {
		t.Error("Should not need snapshot with empty log")
	}

	// Add enough entries to trigger snapshot
	for i := 0; i < 100; i++ {
		rs.AppendLogEntry("command", "test")
	}

	// Now should need snapshot
	if !rs.ShouldTakeSnapshot(100) {
		t.Error("Should need snapshot after 100 entries")
	}
}

func TestGetLastLogIndexWithSnapshot(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1"}

	rs := raft.NewRaftState("node1", peers, applyCh)

	// Add some log entries
	rs.AppendLogEntry("cmd1", "test")
	rs.AppendLogEntry("cmd2", "test")
	rs.AppendLogEntry("cmd3", "test")

	lastIndex := rs.GetLastLogIndexWithSnapshot()
	if lastIndex != 3 {
		t.Errorf("Expected last index 3, got %d", lastIndex)
	}
}

func TestInstallSnapshotRPC(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1"}

	rs := raft.NewRaftState("node1", peers, applyCh)

	// Create snapshot args
	args := &raft.InstallSnapshotArgs{
		Term:              5,
		LeaderID:          "leader",
		LastIncludedIndex: 10,
		LastIncludedTerm:  3,
		Data:              []byte("snapshot data"),
	}

	var reply raft.InstallSnapshotReply
	rs.InstallSnapshot(args, &reply)

	// Check that metadata was updated
	lastIndex, lastTerm := rs.GetSnapshotMetadata()
	if lastIndex != 10 {
		t.Errorf("Expected lastIncludedIndex 10, got %d", lastIndex)
	}
	if lastTerm != 3 {
		t.Errorf("Expected lastIncludedTerm 3, got %d", lastTerm)
	}

	// Check that reply term is correct
	if reply.Term != 5 {
		t.Errorf("Expected reply term 5, got %d", reply.Term)
	}

	// Check that snapshot was sent to apply channel
	select {
	case msg := <-applyCh:
		if !msg.SnapshotValid {
			t.Error("Expected SnapshotValid to be true")
		}
		if msg.SnapshotIndex != 10 {
			t.Errorf("Expected snapshot index 10, got %d", msg.SnapshotIndex)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("Timeout waiting for snapshot apply message")
	}
}

func TestInstallSnapshotDiscardsOldSnapshot(t *testing.T) {
	applyCh := make(chan raft.ApplyMsg, 10)
	peers := []string{"node1"}

	rs := raft.NewRaftState("node1", peers, applyCh)

	// Install first snapshot
	args1 := &raft.InstallSnapshotArgs{
		Term:              5,
		LeaderID:          "leader",
		LastIncludedIndex: 10,
		LastIncludedTerm:  3,
		Data:              []byte("snapshot 1"),
	}
	var reply1 raft.InstallSnapshotReply
	rs.InstallSnapshot(args1, &reply1)

	// Drain apply channel
	<-applyCh

	// Try to install older snapshot (should be rejected)
	args2 := &raft.InstallSnapshotArgs{
		Term:              6,
		LeaderID:          "leader",
		LastIncludedIndex: 5, // Older than current
		LastIncludedTerm:  2,
		Data:              []byte("snapshot 2"),
	}
	var reply2 raft.InstallSnapshotReply
	rs.InstallSnapshot(args2, &reply2)

	// Metadata should not change
	lastIndex, _ := rs.GetSnapshotMetadata()
	if lastIndex != 10 {
		t.Errorf("Metadata should not change for older snapshot: got index=%d", lastIndex)
	}

	// No new message should be sent to apply channel
	select {
	case <-applyCh:
		t.Error("Should not send apply message for older snapshot")
	case <-time.After(50 * time.Millisecond):
		// Expected - no message
	}
}

func TestSnapshotSerializationDeserialization(t *testing.T) {
	args := &raft.InstallSnapshotArgs{
		Term:              5,
		LeaderID:          "leader1",
		LastIncludedIndex: 100,
		LastIncludedTerm:  4,
		Data:              []byte("test snapshot data"),
	}

	// Serialize
	data, err := raft.SerializeInstallSnapshotArgs(args)
	if err != nil {
		t.Fatalf("Failed to serialize: %v", err)
	}

	// Deserialize
	deserialized, err := raft.DeserializeInstallSnapshotArgs(data)
	if err != nil {
		t.Fatalf("Failed to deserialize: %v", err)
	}

	// Verify
	if deserialized.Term != args.Term {
		t.Errorf("Term mismatch: got %d, want %d", deserialized.Term, args.Term)
	}
	if deserialized.LeaderID != args.LeaderID {
		t.Errorf("LeaderID mismatch: got %s, want %s", deserialized.LeaderID, args.LeaderID)
	}
	if deserialized.LastIncludedIndex != args.LastIncludedIndex {
		t.Errorf("LastIncludedIndex mismatch: got %d, want %d", deserialized.LastIncludedIndex, args.LastIncludedIndex)
	}
	if string(deserialized.Data) != string(args.Data) {
		t.Errorf("Data mismatch")
	}
}
