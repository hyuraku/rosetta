package unit

import (
	"bytes"
	"strings"
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
		if _, err := rs.AppendLogEntry("command", "test"); err != nil {
			t.Fatalf("AppendLogEntry: %v", err)
		}
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
	for _, cmd := range []string{"cmd1", "cmd2", "cmd3"} {
		if _, err := rs.AppendLogEntry(cmd, "test"); err != nil {
			t.Fatalf("AppendLogEntry(%s): %v", cmd, err)
		}
	}

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
		Done:              true,
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
		Done:              true,
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
		Done:              true,
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
		Done:              true,
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
	if !bytes.Equal(deserialized.Data, args.Data) {
		t.Errorf("Data mismatch")
	}
	if !deserialized.Done {
		t.Error("Done was lost in the round trip")
	}
}

// TestSnapshotChunkSerializationRoundTrip covers the chunk fields on the wire.
// Serialize/Deserialize are plain json.Marshal/Unmarshal over the whole struct
// (and so is the HTTP transport's sendRPC), so the fields need no work of their
// own — this pins that, and pins the two encodings the `omitempty` tags produce:
// a middle chunk carries "offset" and no "done", while the single-chunk form is
// the pre-chunking message plus "done":true.
func TestSnapshotChunkSerializationRoundTrip(t *testing.T) {
	middle := &raft.InstallSnapshotArgs{
		Term:              5,
		LeaderID:          "leader1",
		LastIncludedIndex: 100,
		LastIncludedTerm:  4,
		Offset:            4096,
		Data:              []byte("a middle chunk"),
	}

	encoded, err := raft.SerializeInstallSnapshotArgs(middle)
	if err != nil {
		t.Fatalf("Failed to serialize: %v", err)
	}
	if strings.Contains(string(encoded), `"done"`) {
		t.Errorf("a non-final chunk encoded a done field: %s", encoded)
	}

	decoded, err := raft.DeserializeInstallSnapshotArgs(encoded)
	if err != nil {
		t.Fatalf("Failed to deserialize: %v", err)
	}
	if decoded.Offset != middle.Offset {
		t.Errorf("Offset mismatch: got %d, want %d", decoded.Offset, middle.Offset)
	}
	if decoded.Done {
		t.Error("Done set on a chunk that did not carry it")
	}
	if !bytes.Equal(decoded.Data, middle.Data) {
		t.Error("Data mismatch")
	}

	// The single-chunk form omits offset entirely, so it is the old whole-payload
	// message plus "done":true.
	single := &raft.InstallSnapshotArgs{
		Term: 5, LeaderID: "leader1", LastIncludedIndex: 100, LastIncludedTerm: 4,
		Data: []byte("whole snapshot"), Done: true,
	}
	encoded, err = raft.SerializeInstallSnapshotArgs(single)
	if err != nil {
		t.Fatalf("Failed to serialize: %v", err)
	}
	if strings.Contains(string(encoded), `"offset"`) {
		t.Errorf("the single-chunk form encoded an offset field: %s", encoded)
	}

	// The reply's offset round-trips the same way.
	replyData, err := raft.SerializeInstallSnapshotReply(&raft.InstallSnapshotReply{Term: 5, Offset: 4096})
	if err != nil {
		t.Fatalf("Failed to serialize reply: %v", err)
	}
	reply, err := raft.DeserializeInstallSnapshotReply(replyData)
	if err != nil {
		t.Fatalf("Failed to deserialize reply: %v", err)
	}
	if reply.Offset != 4096 {
		t.Errorf("reply Offset mismatch: got %d, want 4096", reply.Offset)
	}
}
