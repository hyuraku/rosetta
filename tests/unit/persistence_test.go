package unit

import (
	"os"
	"path/filepath"
	"testing"

	"rosetta/persistence"
	"rosetta/raft"
)

func TestFileStorage_SaveAndLoadRaftState(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer storage.Close()

	// Create test state
	votedFor := "node2"
	testState := &raft.PersistentState{
		CurrentTerm: 5,
		VotedFor:    &votedFor,
		Log: []raft.LogEntry{
			{Term: 1, Index: 1, Command: "cmd1", Type: "command"},
			{Term: 2, Index: 2, Command: "cmd2", Type: "command"},
			{Term: 5, Index: 3, Command: "cmd3", Type: "command"},
		},
	}

	// Save state
	if err := storage.SaveRaftState(testState); err != nil {
		t.Fatalf("Failed to save state: %v", err)
	}

	// Load state
	loadedState, err := storage.LoadRaftState()
	if err != nil {
		t.Fatalf("Failed to load state: %v", err)
	}

	// Verify
	if loadedState.CurrentTerm != testState.CurrentTerm {
		t.Errorf("CurrentTerm mismatch: got %d, want %d", loadedState.CurrentTerm, testState.CurrentTerm)
	}

	if loadedState.VotedFor == nil || *loadedState.VotedFor != *testState.VotedFor {
		t.Errorf("VotedFor mismatch: got %v, want %v", loadedState.VotedFor, testState.VotedFor)
	}

	if len(loadedState.Log) != len(testState.Log) {
		t.Fatalf("Log length mismatch: got %d, want %d", len(loadedState.Log), len(testState.Log))
	}

	for i, entry := range loadedState.Log {
		if entry.Term != testState.Log[i].Term {
			t.Errorf("Log[%d].Term mismatch: got %d, want %d", i, entry.Term, testState.Log[i].Term)
		}
		if entry.Index != testState.Log[i].Index {
			t.Errorf("Log[%d].Index mismatch: got %d, want %d", i, entry.Index, testState.Log[i].Index)
		}
	}
}

func TestFileStorage_EmptyState(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer storage.Close()

	// Load state (should return empty state)
	loadedState, err := storage.LoadRaftState()
	if err != nil {
		t.Fatalf("Failed to load state: %v", err)
	}

	// Verify empty state
	if loadedState.CurrentTerm != 0 {
		t.Errorf("Expected CurrentTerm = 0, got %d", loadedState.CurrentTerm)
	}

	if loadedState.VotedFor != nil {
		t.Errorf("Expected VotedFor = nil, got %v", loadedState.VotedFor)
	}

	if len(loadedState.Log) != 0 {
		t.Errorf("Expected empty log, got %d entries", len(loadedState.Log))
	}
}

func TestFileStorage_SaveAndLoadSnapshot(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer storage.Close()

	// Create test snapshot
	testData := []byte(`{"key1":"value1","key2":"value2"}`)
	testSnapshot := &persistence.Snapshot{
		LastIncludedIndex: 10,
		LastIncludedTerm:  3,
		Data:              testData,
	}

	// Save snapshot
	if err := storage.SaveSnapshot(testSnapshot); err != nil {
		t.Fatalf("Failed to save snapshot: %v", err)
	}

	// Load snapshot
	loadedSnapshot, err := storage.LoadSnapshot()
	if err != nil {
		t.Fatalf("Failed to load snapshot: %v", err)
	}

	// Verify
	if loadedSnapshot.LastIncludedIndex != testSnapshot.LastIncludedIndex {
		t.Errorf("LastIncludedIndex mismatch: got %d, want %d",
			loadedSnapshot.LastIncludedIndex, testSnapshot.LastIncludedIndex)
	}

	if loadedSnapshot.LastIncludedTerm != testSnapshot.LastIncludedTerm {
		t.Errorf("LastIncludedTerm mismatch: got %d, want %d",
			loadedSnapshot.LastIncludedTerm, testSnapshot.LastIncludedTerm)
	}

	if string(loadedSnapshot.Data) != string(testSnapshot.Data) {
		t.Errorf("Data mismatch: got %s, want %s",
			string(loadedSnapshot.Data), string(testSnapshot.Data))
	}
}

func TestFileStorage_NoSnapshot(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer storage.Close()

	// Load snapshot (should return nil)
	loadedSnapshot, err := storage.LoadSnapshot()
	if err != nil {
		t.Fatalf("Failed to load snapshot: %v", err)
	}

	if loadedSnapshot != nil {
		t.Errorf("Expected nil snapshot, got %v", loadedSnapshot)
	}
}

func TestFileStorage_AtomicWrite(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer storage.Close()

	// Save state multiple times
	for term := 1; term <= 10; term++ {
		state := &raft.PersistentState{
			CurrentTerm: term,
			VotedFor:    nil,
			Log:         make([]raft.LogEntry, 0),
		}

		if err := storage.SaveRaftState(state); err != nil {
			t.Fatalf("Failed to save state at term %d: %v", term, err)
		}

		// Verify no temporary files left behind
		files, err := storage.ListFiles()
		if err != nil {
			t.Fatalf("Failed to list files: %v", err)
		}

		for _, file := range files {
			if filepath.Ext(file) == ".tmp" {
				t.Errorf("Temporary file not cleaned up: %s", file)
			}
		}
	}

	// Verify final state
	finalState, err := storage.LoadRaftState()
	if err != nil {
		t.Fatalf("Failed to load final state: %v", err)
	}

	if finalState.CurrentTerm != 10 {
		t.Errorf("Expected final term = 10, got %d", finalState.CurrentTerm)
	}
}

func TestKVSnapshotter_SaveAndLoad(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer storage.Close()

	// Create snapshotter
	snapshotter := persistence.NewKVSnapshotter(storage)

	// Test data
	testData := map[string]string{
		"key1": "value1",
		"key2": "value2",
		"key3": "value3",
	}

	// Save snapshot
	if err := snapshotter.SaveSnapshot(testData, 100, 5); err != nil {
		t.Fatalf("Failed to save snapshot: %v", err)
	}

	// Load snapshot
	loadedData, lastIndex, lastTerm, err := snapshotter.LoadSnapshot()
	if err != nil {
		t.Fatalf("Failed to load snapshot: %v", err)
	}

	// Verify
	if lastIndex != 100 {
		t.Errorf("LastIndex mismatch: got %d, want 100", lastIndex)
	}

	if lastTerm != 5 {
		t.Errorf("LastTerm mismatch: got %d, want 5", lastTerm)
	}

	if len(loadedData) != len(testData) {
		t.Fatalf("Data length mismatch: got %d, want %d", len(loadedData), len(testData))
	}

	for key, value := range testData {
		if loadedData[key] != value {
			t.Errorf("Data[%s] mismatch: got %s, want %s", key, loadedData[key], value)
		}
	}
}

func TestRaftPersister_Integration(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()

	// Create storage
	storage, err := persistence.NewFileStorage(tempDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}
	defer storage.Close()

	// Create persister
	persister := persistence.NewRaftPersister(storage)

	// Save multiple states
	for term := 1; term <= 5; term++ {
		votedFor := "node1"
		state := &raft.PersistentState{
			CurrentTerm: term,
			VotedFor:    &votedFor,
			Log: []raft.LogEntry{
				{Term: term, Index: term, Command: "cmd", Type: "command"},
			},
		}

		if err := persister.SaveRaftState(state); err != nil {
			t.Fatalf("Failed to save state at term %d: %v", term, err)
		}
	}

	// Load final state
	finalState, err := persister.LoadRaftState()
	if err != nil {
		t.Fatalf("Failed to load state: %v", err)
	}

	if finalState.CurrentTerm != 5 {
		t.Errorf("Expected term = 5, got %d", finalState.CurrentTerm)
	}

	if len(finalState.Log) != 1 {
		t.Errorf("Expected 1 log entry, got %d", len(finalState.Log))
	}
}

func TestFileStorage_CopyTo(t *testing.T) {
	// Create temporary directories
	srcDir := t.TempDir()
	destDir := t.TempDir()

	// Create source storage
	srcStorage, err := persistence.NewFileStorage(srcDir)
	if err != nil {
		t.Fatalf("Failed to create source storage: %v", err)
	}
	defer srcStorage.Close()

	// Save some data
	votedFor := "node1"
	testState := &raft.PersistentState{
		CurrentTerm: 10,
		VotedFor:    &votedFor,
		Log:         []raft.LogEntry{{Term: 1, Index: 1, Command: "test", Type: "command"}},
	}

	if err := srcStorage.SaveRaftState(testState); err != nil {
		t.Fatalf("Failed to save state: %v", err)
	}

	testSnapshot := &persistence.Snapshot{
		LastIncludedIndex: 5,
		LastIncludedTerm:  2,
		Data:              []byte("test data"),
	}

	if err := srcStorage.SaveSnapshot(testSnapshot); err != nil {
		t.Fatalf("Failed to save snapshot: %v", err)
	}

	// Copy to destination
	if err := srcStorage.CopyTo(destDir); err != nil {
		t.Fatalf("Failed to copy: %v", err)
	}

	// Verify destination
	destStorage, err := persistence.NewFileStorage(destDir)
	if err != nil {
		t.Fatalf("Failed to create dest storage: %v", err)
	}
	defer destStorage.Close()

	// Load and verify state
	loadedState, err := destStorage.LoadRaftState()
	if err != nil {
		t.Fatalf("Failed to load state from dest: %v", err)
	}

	if loadedState.CurrentTerm != testState.CurrentTerm {
		t.Errorf("State not copied correctly")
	}

	// Load and verify snapshot
	loadedSnapshot, err := destStorage.LoadSnapshot()
	if err != nil {
		t.Fatalf("Failed to load snapshot from dest: %v", err)
	}

	if loadedSnapshot.LastIncludedIndex != testSnapshot.LastIncludedIndex {
		t.Errorf("Snapshot not copied correctly")
	}
}

func TestFileStorage_DeleteAll(t *testing.T) {
	// Create temporary directory
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "data")

	// Create storage
	storage, err := persistence.NewFileStorage(dataDir)
	if err != nil {
		t.Fatalf("Failed to create storage: %v", err)
	}

	// Save some data
	state := &raft.PersistentState{
		CurrentTerm: 1,
		VotedFor:    nil,
		Log:         make([]raft.LogEntry, 0),
	}

	if err := storage.SaveRaftState(state); err != nil {
		t.Fatalf("Failed to save state: %v", err)
	}

	// Verify directory exists
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Fatalf("Data directory should exist")
	}

	// Delete all
	if err := storage.DeleteAll(); err != nil {
		t.Fatalf("Failed to delete all: %v", err)
	}

	// Verify directory is gone
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Errorf("Data directory should be deleted")
	}

	storage.Close()
}
