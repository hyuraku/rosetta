package kvstore

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"rosetta/raft"
)

type Operation string

const (
	OpPut    Operation = "PUT"
	OpGet    Operation = "GET"
	OpDelete Operation = "DELETE"
)

type Command struct {
	Op    Operation `json:"op"`
	Key   string    `json:"key"`
	Value string    `json:"value,omitempty"`
	ID    string    `json:"id"`

	// Duplicate detection fields (Raft paper Section 8)
	ClientID string `json:"client_id,omitempty"` // Unique client identifier
	SeqNum   int    `json:"seq_num,omitempty"`   // Monotonically increasing sequence number
}

type Result struct {
	Value string `json:"value"`
	Err   error  `json:"error"`
}

// ClientSession tracks the last operation from each client for duplicate detection
type ClientSession struct {
	LastSeqNum int    `json:"last_seq_num"` // Last executed sequence number
	LastResult Result `json:"last_result"`  // Cached result for duplicate requests
}

// SnapshotData contains all data to be persisted in a snapshot
type SnapshotData struct {
	KVData   map[string]string          `json:"kv_data"`
	Sessions map[string]*ClientSession  `json:"sessions,omitempty"` // For duplicate detection
}

// Snapshotter interface for saving/loading snapshots
type Snapshotter interface {
	SaveSnapshot(data map[string]string, lastIncludedIndex, lastIncludedTerm int) error
	LoadSnapshot() (data map[string]string, lastIncludedIndex, lastIncludedTerm int, err error)
}

// SnapshotterV2 extends Snapshotter to support session data for duplicate detection
type SnapshotterV2 interface {
	Snapshotter
	SaveSnapshotV2(data *SnapshotData, lastIncludedIndex, lastIncludedTerm int) error
	LoadSnapshotV2() (data *SnapshotData, lastIncludedIndex, lastIncludedTerm int, err error)
}

type KVStore struct {
	mu      sync.RWMutex
	data    map[string]string
	raft    *raft.RaftNode
	applyCh chan raft.ApplyMsg

	pendingOps map[string]chan Result
	opMu       sync.RWMutex

	// Duplicate detection (Raft paper Section 8)
	sessions  map[string]*ClientSession // ClientID -> Session
	sessionMu sync.RWMutex

	maxRaftState     int
	snapshotter      Snapshotter
	lastAppliedIndex int
	lastAppliedTerm  int
	logger           *log.Logger
}

func NewKVStore(maxRaftState int) *KVStore {
	return NewKVStoreWithSnapshotter(maxRaftState, nil)
}

func NewKVStoreWithSnapshotter(maxRaftState int, snapshotter Snapshotter) *KVStore {
	applyCh := make(chan raft.ApplyMsg, 100)

	kvs := &KVStore{
		data:             make(map[string]string),
		applyCh:          applyCh,
		pendingOps:       make(map[string]chan Result),
		sessions:         make(map[string]*ClientSession),
		maxRaftState:     maxRaftState,
		snapshotter:      snapshotter,
		lastAppliedIndex: 0,
		lastAppliedTerm:  0,
		logger:           log.New(log.Writer(), "[KVSTORE] ", log.LstdFlags),
	}

	// Load snapshot if available
	if snapshotter != nil {
		if err := kvs.loadSnapshot(); err != nil {
			kvs.logger.Printf("Warning: failed to load snapshot: %v", err)
		}
	}

	go kvs.applyLoop()
	return kvs
}

// loadSnapshot loads the snapshot from storage
func (kvs *KVStore) loadSnapshot() error {
	// Try V2 interface first for session support
	if snapshotterV2, ok := kvs.snapshotter.(SnapshotterV2); ok {
		snapshotData, lastIndex, lastTerm, err := snapshotterV2.LoadSnapshotV2()
		if err != nil {
			return err
		}

		if snapshotData != nil {
			kvs.mu.Lock()
			kvs.data = snapshotData.KVData
			kvs.lastAppliedIndex = lastIndex
			kvs.lastAppliedTerm = lastTerm
			kvs.mu.Unlock()

			// Restore sessions for duplicate detection
			if snapshotData.Sessions != nil {
				kvs.sessionMu.Lock()
				kvs.sessions = snapshotData.Sessions
				kvs.sessionMu.Unlock()
			}

			kvs.logger.Printf("Loaded snapshot V2: kvEntries=%d, sessions=%d, lastIndex=%d, lastTerm=%d",
				len(snapshotData.KVData), len(snapshotData.Sessions), lastIndex, lastTerm)
		}
		return nil
	}

	// Fallback to V1 interface
	data, lastIndex, lastTerm, err := kvs.snapshotter.LoadSnapshot()
	if err != nil {
		return err
	}

	if data != nil {
		kvs.mu.Lock()
		kvs.data = data
		kvs.lastAppliedIndex = lastIndex
		kvs.lastAppliedTerm = lastTerm
		kvs.mu.Unlock()
		kvs.logger.Printf("Loaded snapshot: entries=%d, lastIndex=%d, lastTerm=%d", len(data), lastIndex, lastTerm)
	}

	return nil
}

// saveSnapshot saves the current state to a snapshot
func (kvs *KVStore) saveSnapshot(lastIndex, lastTerm int) error {
	if kvs.snapshotter == nil {
		return nil
	}

	kvs.mu.RLock()
	dataCopy := make(map[string]string, len(kvs.data))
	for k, v := range kvs.data {
		dataCopy[k] = v
	}
	kvs.mu.RUnlock()

	// Copy session data for duplicate detection
	kvs.sessionMu.RLock()
	sessionsCopy := make(map[string]*ClientSession, len(kvs.sessions))
	for clientID, session := range kvs.sessions {
		sessionsCopy[clientID] = &ClientSession{
			LastSeqNum: session.LastSeqNum,
			LastResult: session.LastResult,
		}
	}
	kvs.sessionMu.RUnlock()

	// Use V2 interface if available for session persistence
	if snapshotterV2, ok := kvs.snapshotter.(SnapshotterV2); ok {
		snapshotData := &SnapshotData{
			KVData:   dataCopy,
			Sessions: sessionsCopy,
		}
		if err := snapshotterV2.SaveSnapshotV2(snapshotData, lastIndex, lastTerm); err != nil {
			return err
		}
		kvs.logger.Printf("Saved snapshot V2: kvEntries=%d, sessions=%d, lastIndex=%d, lastTerm=%d",
			len(dataCopy), len(sessionsCopy), lastIndex, lastTerm)
		return nil
	}

	// Fallback to V1 interface (sessions not persisted)
	if err := kvs.snapshotter.SaveSnapshot(dataCopy, lastIndex, lastTerm); err != nil {
		return err
	}

	kvs.logger.Printf("Saved snapshot: entries=%d, lastIndex=%d, lastTerm=%d", len(dataCopy), lastIndex, lastTerm)
	return nil
}

func (kvs *KVStore) SetRaft(raftNode *raft.RaftNode) {
	kvs.raft = raftNode
}

func (kvs *KVStore) applyLoop() {
	commandsSinceSnapshot := 0

	for applyMsg := range kvs.applyCh {
		// Handle snapshot installation
		if applyMsg.SnapshotValid {
			kvs.installSnapshotFromApplyMsg(&applyMsg)
			commandsSinceSnapshot = 0
			continue
		}

		if !applyMsg.CommandValid {
			continue
		}

		var cmd Command
		if err := json.Unmarshal([]byte(fmt.Sprintf("%v", applyMsg.Command)), &cmd); err != nil {
			cmdBytes, ok := applyMsg.Command.([]byte)
			if !ok {
				continue
			}
			if err := json.Unmarshal(cmdBytes, &cmd); err != nil {
				continue
			}
		}

		result := kvs.executeCommand(&cmd)

		// Update last applied index
		kvs.mu.Lock()
		kvs.lastAppliedIndex = applyMsg.CommandIndex
		kvs.mu.Unlock()

		kvs.opMu.RLock()
		if ch, exists := kvs.pendingOps[cmd.ID]; exists {
			select {
			case ch <- result:
			default:
			}
			delete(kvs.pendingOps, cmd.ID)
		}
		kvs.opMu.RUnlock()

		// Check if we should take a snapshot
		commandsSinceSnapshot++
		if kvs.maxRaftState > 0 && commandsSinceSnapshot >= kvs.maxRaftState {
			kvs.mu.RLock()
			lastIndex := kvs.lastAppliedIndex
			lastTerm := kvs.lastAppliedTerm
			kvs.mu.RUnlock()

			if err := kvs.saveSnapshot(lastIndex, lastTerm); err != nil {
				kvs.logger.Printf("Failed to save snapshot: %v", err)
			} else {
				// Notify Raft to compact log
				if kvs.raft != nil {
					kvs.raft.TriggerSnapshot(lastIndex)
				}
				commandsSinceSnapshot = 0
			}
		}
	}
}

// installSnapshotFromApplyMsg installs a snapshot received via apply channel
func (kvs *KVStore) installSnapshotFromApplyMsg(msg *raft.ApplyMsg) {
	// Deserialize snapshot data
	var snapshotData map[string]string
	if err := json.Unmarshal(msg.SnapshotData, &snapshotData); err != nil {
		kvs.logger.Printf("Failed to unmarshal snapshot data: %v", err)
		return
	}

	kvs.mu.Lock()
	kvs.data = snapshotData
	kvs.lastAppliedIndex = msg.SnapshotIndex
	kvs.lastAppliedTerm = msg.SnapshotTerm
	kvs.mu.Unlock()

	kvs.logger.Printf("Installed snapshot: entries=%d, lastIndex=%d, lastTerm=%d",
		len(snapshotData), msg.SnapshotIndex, msg.SnapshotTerm)
}

func (kvs *KVStore) executeCommand(cmd *Command) Result {
	// Duplicate detection (Raft paper Section 8)
	// If ClientID is set, check for duplicate requests
	if cmd.ClientID != "" {
		kvs.sessionMu.RLock()
		session, exists := kvs.sessions[cmd.ClientID]
		kvs.sessionMu.RUnlock()

		if exists {
			if cmd.SeqNum < session.LastSeqNum {
				// Stale request - already processed a newer one
				kvs.logger.Printf("Stale request from client %s: seqNum=%d < lastSeqNum=%d",
					cmd.ClientID, cmd.SeqNum, session.LastSeqNum)
				return Result{Value: "", Err: fmt.Errorf("stale request")}
			}
			if cmd.SeqNum == session.LastSeqNum {
				// Duplicate request - return cached result
				kvs.logger.Printf("Duplicate request from client %s: seqNum=%d (returning cached result)",
					cmd.ClientID, cmd.SeqNum)
				return session.LastResult
			}
			// cmd.SeqNum > session.LastSeqNum: new request, proceed
		}
	}

	// Execute the actual operation
	kvs.mu.Lock()
	var result Result
	switch cmd.Op {
	case OpPut:
		kvs.data[cmd.Key] = cmd.Value
		result = Result{Value: "", Err: nil}
	case OpGet:
		value, exists := kvs.data[cmd.Key]
		if !exists {
			result = Result{Value: "", Err: fmt.Errorf("key not found")}
		} else {
			result = Result{Value: value, Err: nil}
		}
	case OpDelete:
		delete(kvs.data, cmd.Key)
		result = Result{Value: "", Err: nil}
	default:
		result = Result{Value: "", Err: fmt.Errorf("unknown operation")}
	}
	kvs.mu.Unlock()

	// Update session for duplicate detection
	if cmd.ClientID != "" {
		kvs.sessionMu.Lock()
		kvs.sessions[cmd.ClientID] = &ClientSession{
			LastSeqNum: cmd.SeqNum,
			LastResult: result,
		}
		kvs.sessionMu.Unlock()
	}

	return result
}

func (kvs *KVStore) Put(key, value string) error {
	return kvs.executeOperation(OpPut, key, value)
}

func (kvs *KVStore) Get(key string) (string, error) {
	if kvs.raft == nil {
		return "", fmt.Errorf("raft node not initialized")
	}

	// Read-only optimization (Raft paper Section 8):
	// If the leader has recently confirmed leadership via heartbeats,
	// we can serve reads directly from local state without going through Raft.
	if kvs.raft.CanServeReadOnlyQuery() {
		return kvs.getLocal(key)
	}

	// Fall back to going through Raft for strong consistency
	// This happens when:
	// 1. This node is not the leader
	// 2. The leader hasn't received heartbeat confirmations from a majority recently
	result := kvs.executeOperationWithResult(OpGet, key, "")
	if result.Err != nil {
		return "", result.Err
	}
	return result.Value, nil
}

// getLocal reads directly from local state.
// This should only be called when CanServeReadOnlyQuery() returns true.
func (kvs *KVStore) getLocal(key string) (string, error) {
	kvs.mu.RLock()
	defer kvs.mu.RUnlock()

	value, exists := kvs.data[key]
	if !exists {
		return "", fmt.Errorf("key not found")
	}
	return value, nil
}

func (kvs *KVStore) Delete(key string) error {
	return kvs.executeOperation(OpDelete, key, "")
}

func (kvs *KVStore) executeOperation(op Operation, key, value string) error {
	result := kvs.executeOperationWithResult(op, key, value)
	return result.Err
}

func (kvs *KVStore) executeOperationWithResult(op Operation, key, value string) Result {
	if kvs.raft == nil {
		return Result{Value: "", Err: fmt.Errorf("raft node not initialized")}
	}

	if !kvs.raft.IsLeader() {
		return Result{Value: "", Err: fmt.Errorf("not leader")}
	}

	opID := fmt.Sprintf("%s-%d", kvs.raft.GetNodeID(), time.Now().UnixNano())
	cmd := Command{
		Op:    op,
		Key:   key,
		Value: value,
		ID:    opID,
	}

	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return Result{Value: "", Err: err}
	}

	_, term, isLeader := kvs.raft.Start(string(cmdBytes))
	if !isLeader {
		return Result{Value: "", Err: fmt.Errorf("not leader")}
	}

	resultCh := make(chan Result, 1)
	kvs.opMu.Lock()
	kvs.pendingOps[opID] = resultCh
	kvs.opMu.Unlock()

	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()

	select {
	case result := <-resultCh:
		currentTerm, stillLeader := kvs.raft.GetState()
		if !stillLeader || currentTerm != term {
			return Result{Value: "", Err: fmt.Errorf("leadership lost")}
		}
		return result
	case <-timeout.C:
		kvs.opMu.Lock()
		delete(kvs.pendingOps, opID)
		kvs.opMu.Unlock()
		return Result{Value: "", Err: fmt.Errorf("operation timeout")}
	}
}

func (kvs *KVStore) GetSnapshot() map[string]string {
	kvs.mu.RLock()
	defer kvs.mu.RUnlock()

	snapshot := make(map[string]string)
	for k, v := range kvs.data {
		snapshot[k] = v
	}
	return snapshot
}

func (kvs *KVStore) RestoreSnapshot(snapshot map[string]string) {
	kvs.mu.Lock()
	defer kvs.mu.Unlock()

	kvs.data = make(map[string]string)
	for k, v := range snapshot {
		kvs.data[k] = v
	}
}

func (kvs *KVStore) Size() int {
	kvs.mu.RLock()
	defer kvs.mu.RUnlock()
	return len(kvs.data)
}

func (kvs *KVStore) Keys() []string {
	kvs.mu.RLock()
	defer kvs.mu.RUnlock()

	keys := make([]string, 0, len(kvs.data))
	for k := range kvs.data {
		keys = append(keys, k)
	}
	return keys
}

func (kvs *KVStore) Clear() {
	kvs.mu.Lock()
	defer kvs.mu.Unlock()
	kvs.data = make(map[string]string)
}

func (kvs *KVStore) GetApplyCh() chan raft.ApplyMsg {
	return kvs.applyCh
}

func (kvs *KVStore) Close() {
	close(kvs.applyCh)
}
