package persistence

import (
	"encoding/json"
	"fmt"

	"rosetta/kvstore"
)

// KVSnapshotter implements the kvstore.Snapshotter and kvstore.SnapshotterV2 interfaces
type KVSnapshotter struct {
	storage Storage
}

// NewKVSnapshotter creates a new KV snapshotter
func NewKVSnapshotter(storage Storage) *KVSnapshotter {
	return &KVSnapshotter{storage: storage}
}

// saveSnapshotWithData is a helper that saves any data type as a snapshot
func (ks *KVSnapshotter) saveSnapshotWithData(data interface{}, lastIncludedIndex, lastIncludedTerm int) error {
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

// SaveSnapshot saves the KV store data as a snapshot (V1 interface)
func (ks *KVSnapshotter) SaveSnapshot(data map[string]string, lastIncludedIndex, lastIncludedTerm int) error {
	return ks.saveSnapshotWithData(data, lastIncludedIndex, lastIncludedTerm)
}

// SaveSnapshotV2 saves the KV store data with session information as a snapshot
func (ks *KVSnapshotter) SaveSnapshotV2(data *kvstore.SnapshotData, lastIncludedIndex, lastIncludedTerm int) error {
	return ks.saveSnapshotWithData(data, lastIncludedIndex, lastIncludedTerm)
}

// LoadSnapshot loads the KV store data from a snapshot (V1 interface)
func (ks *KVSnapshotter) LoadSnapshot() (map[string]string, int, int, error) {
	snapshot, err := ks.storage.LoadSnapshot()
	if err != nil || snapshot == nil {
		return nil, 0, 0, err
	}

	var kvData map[string]string
	if err := json.Unmarshal(snapshot.Data, &kvData); err != nil {
		return nil, 0, 0, fmt.Errorf("failed to unmarshal snapshot data: %w", err)
	}

	return kvData, snapshot.LastIncludedIndex, snapshot.LastIncludedTerm, nil
}

// LoadSnapshotV2 loads the KV store data with session information from a snapshot
func (ks *KVSnapshotter) LoadSnapshotV2() (*kvstore.SnapshotData, int, int, error) {
	snapshot, err := ks.storage.LoadSnapshot()
	if err != nil || snapshot == nil {
		return nil, 0, 0, err
	}

	// Try V2 format first (with sessions)
	var snapshotData kvstore.SnapshotData
	if err := json.Unmarshal(snapshot.Data, &snapshotData); err == nil && snapshotData.KVData != nil {
		return &snapshotData, snapshot.LastIncludedIndex, snapshot.LastIncludedTerm, nil
	}

	// Fallback to V1 format (plain map[string]string)
	var kvData map[string]string
	if err := json.Unmarshal(snapshot.Data, &kvData); err != nil {
		return nil, 0, 0, fmt.Errorf("failed to unmarshal snapshot data: %w", err)
	}

	return &kvstore.SnapshotData{
		KVData:   kvData,
		Sessions: nil,
	}, snapshot.LastIncludedIndex, snapshot.LastIncludedTerm, nil
}
