package persistence

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"rosetta/raft"
)

const (
	stateFileName    = "raft_state.json"
	snapshotFileName = "snapshot.json"
	tempSuffix       = ".tmp"
)

// FileStorage implements Storage interface using file system
type FileStorage struct {
	dataDir string
	mu      sync.RWMutex
}

// NewFileStorage creates a new file-based storage
func NewFileStorage(dataDir string) (*FileStorage, error) {
	// Create data directory if it doesn't exist
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	return &FileStorage{
		dataDir: dataDir,
	}, nil
}

// SaveRaftState saves the Raft state to disk atomically
func (fs *FileStorage) SaveRaftState(state *raft.PersistentState) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	statePath := filepath.Join(fs.dataDir, stateFileName)
	tempPath := statePath + tempSuffix

	// Marshal state to JSON
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal raft state: %w", err)
	}

	// Write to temporary file first
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temporary state file: %w", err)
	}

	// Sync to ensure data is on disk
	if err := syncFile(tempPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to sync temporary state file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, statePath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to rename state file: %w", err)
	}

	// Sync directory to ensure rename is persisted
	if err := syncDir(fs.dataDir); err != nil {
		return fmt.Errorf("failed to sync data directory: %w", err)
	}

	return nil
}

// LoadRaftState loads the Raft state from disk
func (fs *FileStorage) LoadRaftState() (*raft.PersistentState, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	statePath := filepath.Join(fs.dataDir, stateFileName)

	// Check if file exists
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		// Return empty state if file doesn't exist
		return &raft.PersistentState{
			CurrentTerm: 0,
			VotedFor:    nil,
			Log:         make([]raft.LogEntry, 0),
		}, nil
	}

	// Read state file
	data, err := os.ReadFile(statePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read state file: %w", err)
	}

	// Unmarshal state
	var state raft.PersistentState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal raft state: %w", err)
	}

	// Ensure log is not nil
	if state.Log == nil {
		state.Log = make([]raft.LogEntry, 0)
	}

	return &state, nil
}

// SaveSnapshot saves a snapshot to disk atomically
func (fs *FileStorage) SaveSnapshot(snapshot *Snapshot) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	snapshotPath := filepath.Join(fs.dataDir, snapshotFileName)
	tempPath := snapshotPath + tempSuffix

	// Marshal snapshot to JSON
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	// Write to temporary file first
	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temporary snapshot file: %w", err)
	}

	// Sync to ensure data is on disk
	if err := syncFile(tempPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to sync temporary snapshot file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, snapshotPath); err != nil {
		os.Remove(tempPath)
		return fmt.Errorf("failed to rename snapshot file: %w", err)
	}

	// Sync directory to ensure rename is persisted
	if err := syncDir(fs.dataDir); err != nil {
		return fmt.Errorf("failed to sync data directory: %w", err)
	}

	return nil
}

// LoadSnapshot loads the snapshot from disk
func (fs *FileStorage) LoadSnapshot() (*Snapshot, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	snapshotPath := filepath.Join(fs.dataDir, snapshotFileName)

	// Check if file exists
	if _, err := os.Stat(snapshotPath); os.IsNotExist(err) {
		return nil, nil // No snapshot exists yet
	}

	// Read snapshot file
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read snapshot file: %w", err)
	}

	// Unmarshal snapshot
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("failed to unmarshal snapshot: %w", err)
	}

	return &snapshot, nil
}

// Close closes the file storage
func (fs *FileStorage) Close() error {
	// No resources to clean up for file storage
	return nil
}

// syncFile syncs a file to disk
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Sync(); err != nil {
		return err
	}

	return nil
}

// syncDir syncs a directory to disk
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Sync(); err != nil {
		return err
	}

	return nil
}

// GetDataDir returns the data directory path
func (fs *FileStorage) GetDataDir() string {
	return fs.dataDir
}

// DeleteAll removes all persisted data (useful for testing)
func (fs *FileStorage) DeleteAll() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	return os.RemoveAll(fs.dataDir)
}

// ListFiles returns a list of persisted files
func (fs *FileStorage) ListFiles() ([]string, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	files := make([]string, 0)

	entries, err := os.ReadDir(fs.dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return files, nil
		}
		return nil, fmt.Errorf("failed to read data directory: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
	}

	return files, nil
}

// CopyTo copies all persisted data to another directory (for backup)
func (fs *FileStorage) CopyTo(destDir string) error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	// Create destination directory
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Copy state file if exists
	statePath := filepath.Join(fs.dataDir, stateFileName)
	if _, err := os.Stat(statePath); err == nil {
		if err := copyFile(statePath, filepath.Join(destDir, stateFileName)); err != nil {
			return fmt.Errorf("failed to copy state file: %w", err)
		}
	}

	// Copy snapshot file if exists
	snapshotPath := filepath.Join(fs.dataDir, snapshotFileName)
	if _, err := os.Stat(snapshotPath); err == nil {
		if err := copyFile(snapshotPath, filepath.Join(destDir, snapshotFileName)); err != nil {
			return fmt.Errorf("failed to copy snapshot file: %w", err)
		}
	}

	return nil
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return err
	}

	return dstFile.Sync()
}
