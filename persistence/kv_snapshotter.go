package persistence

import (
	"encoding/json"
	"fmt"

	"rosetta/kvstore"
)

// KVSnapshotter implements the kvstore.Snapshotter interface
type KVSnapshotter struct {
	storage Storage
}

// NewKVSnapshotter creates a new KV snapshotter
func NewKVSnapshotter(storage Storage) *KVSnapshotter {
	return &KVSnapshotter{
		storage: storage,
	}
}

// SaveSnapshot saves the KV store data as a snapshot
func (ks *KVSnapshotter) SaveSnapshot(data map[string]string, lastIncludedIndex, lastIncludedTerm int) error {
	// Marshal the data
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot data: %w", err)
	}

	snapshot := &Snapshot{
		LastIncludedIndex: lastIncludedIndex,
		LastIncludedTerm:  lastIncludedTerm,
		Data:              dataBytes,
	}

	return ks.storage.SaveSnapshot(snapshot)
}

// LoadSnapshot loads the KV store data from a snapshot
func (ks *KVSnapshotter) LoadSnapshot() (data map[string]string, lastIncludedIndex, lastIncludedTerm int, err error) {
	snapshot, err := ks.storage.LoadSnapshot()
	if err != nil {
		return nil, 0, 0, err
	}

	if snapshot == nil {
		return nil, 0, 0, nil
	}

	// Unmarshal the data
	var kvData map[string]string
	if err := json.Unmarshal(snapshot.Data, &kvData); err != nil {
		return nil, 0, 0, fmt.Errorf("failed to unmarshal snapshot data: %w", err)
	}

	return kvData, snapshot.LastIncludedIndex, snapshot.LastIncludedTerm, nil
}

// SaveSnapshotV2 saves the KV store data with session information as a snapshot
// This implements the kvstore.SnapshotterV2 interface for duplicate detection support
func (ks *KVSnapshotter) SaveSnapshotV2(data *kvstore.SnapshotData, lastIncludedIndex, lastIncludedTerm int) error {
	// Marshal the full snapshot data including sessions
	dataBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal snapshot data V2: %w", err)
	}

	snapshot := &Snapshot{
		LastIncludedIndex: lastIncludedIndex,
		LastIncludedTerm:  lastIncludedTerm,
		Data:              dataBytes,
	}

	return ks.storage.SaveSnapshot(snapshot)
}

// LoadSnapshotV2 loads the KV store data with session information from a snapshot
// This implements the kvstore.SnapshotterV2 interface for duplicate detection support
func (ks *KVSnapshotter) LoadSnapshotV2() (data *kvstore.SnapshotData, lastIncludedIndex, lastIncludedTerm int, err error) {
	snapshot, err := ks.storage.LoadSnapshot()
	if err != nil {
		return nil, 0, 0, err
	}

	if snapshot == nil {
		return nil, 0, 0, nil
	}

	// Try to unmarshal as V2 format first (with sessions)
	var snapshotData kvstore.SnapshotData
	if err := json.Unmarshal(snapshot.Data, &snapshotData); err == nil && snapshotData.KVData != nil {
		return &snapshotData, snapshot.LastIncludedIndex, snapshot.LastIncludedTerm, nil
	}

	// Fallback: try to unmarshal as V1 format (plain map[string]string)
	var kvData map[string]string
	if err := json.Unmarshal(snapshot.Data, &kvData); err != nil {
		return nil, 0, 0, fmt.Errorf("failed to unmarshal snapshot data: %w", err)
	}

	// Convert V1 to V2 format
	return &kvstore.SnapshotData{
		KVData:   kvData,
		Sessions: nil,
	}, snapshot.LastIncludedIndex, snapshot.LastIncludedTerm, nil
}
