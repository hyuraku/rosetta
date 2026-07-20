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

const (
	// applyChBufferSize is the buffer size for the apply channel bridging Raft and the KV store.
	applyChBufferSize = 100
	// operationTimeout is how long a pending operation waits to be committed and applied.
	operationTimeout = 5 * time.Second
	// applyWaitPollInterval is how often a linearizable read polls the applied
	// index while waiting to catch up to a ReadIndex.
	applyWaitPollInterval = 2 * time.Millisecond
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
	KVData   map[string]string         `json:"kv_data"`
	Sessions map[string]*ClientSession `json:"sessions,omitempty"` // For duplicate detection
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
	applyCh := make(chan raft.ApplyMsg, applyChBufferSize)

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
		return kvs.loadSnapshotV2(snapshotterV2)
	}
	return kvs.loadSnapshotV1()
}

// loadSnapshotV2 loads snapshot with session data for duplicate detection
func (kvs *KVStore) loadSnapshotV2(snapshotter SnapshotterV2) error {
	snapshotData, lastIndex, lastTerm, err := snapshotter.LoadSnapshotV2()
	if err != nil {
		return err
	}
	if snapshotData == nil {
		return nil
	}

	kvs.mu.Lock()
	kvs.data = snapshotData.KVData
	kvs.lastAppliedIndex = lastIndex
	kvs.lastAppliedTerm = lastTerm
	kvs.mu.Unlock()

	if snapshotData.Sessions != nil {
		kvs.sessionMu.Lock()
		kvs.sessions = snapshotData.Sessions
		kvs.sessionMu.Unlock()
	}

	kvs.logger.Printf("Loaded snapshot V2: kvEntries=%d, sessions=%d, lastIndex=%d, lastTerm=%d",
		len(snapshotData.KVData), len(snapshotData.Sessions), lastIndex, lastTerm)
	return nil
}

// loadSnapshotV1 loads snapshot without session data (backward compatibility)
func (kvs *KVStore) loadSnapshotV1() error {
	data, lastIndex, lastTerm, err := kvs.snapshotter.LoadSnapshot()
	if err != nil {
		return err
	}
	if data == nil {
		return nil
	}

	kvs.mu.Lock()
	kvs.data = data
	kvs.lastAppliedIndex = lastIndex
	kvs.lastAppliedTerm = lastTerm
	kvs.mu.Unlock()

	kvs.logger.Printf("Loaded snapshot: entries=%d, lastIndex=%d, lastTerm=%d", len(data), lastIndex, lastTerm)
	return nil
}

// saveSnapshot saves the current state to a snapshot
func (kvs *KVStore) saveSnapshot(lastIndex, lastTerm int) error {
	if kvs.snapshotter == nil {
		return nil
	}

	dataCopy := kvs.copyKVData()
	sessionsCopy := kvs.copySessionData()

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

// copyKVData returns a deep copy of the KV data
func (kvs *KVStore) copyKVData() map[string]string {
	kvs.mu.RLock()
	defer kvs.mu.RUnlock()

	dataCopy := make(map[string]string, len(kvs.data))
	for k, v := range kvs.data {
		dataCopy[k] = v
	}
	return dataCopy
}

// copySessionData returns a deep copy of the session data
func (kvs *KVStore) copySessionData() map[string]*ClientSession {
	kvs.sessionMu.RLock()
	defer kvs.sessionMu.RUnlock()

	sessionsCopy := make(map[string]*ClientSession, len(kvs.sessions))
	for clientID, session := range kvs.sessions {
		sessionsCopy[clientID] = &ClientSession{
			LastSeqNum: session.LastSeqNum,
			LastResult: session.LastResult,
		}
	}
	return sessionsCopy
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

		// A no-op entry (appended by a newly elected leader, Raft §6.4) carries
		// no state-machine command. We must still advance lastAppliedIndex over
		// it — both the log-compaction accounting and the ReadIndex catch-up in
		// waitForApplied rely on lastAppliedIndex reflecting every committed
		// index — then skip execution and pendingOps matching.
		if isNoOpCommand(applyMsg.Command) {
			kvs.mu.Lock()
			kvs.lastAppliedIndex = applyMsg.CommandIndex
			kvs.lastAppliedTerm = applyMsg.CommandTerm
			kvs.mu.Unlock()
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

		// Update last applied index and term. CommandTerm is the absolute term
		// of the applied entry; tracking it keeps saveSnapshot's LastIncludedTerm
		// correct when a snapshot is taken from this loop.
		kvs.mu.Lock()
		kvs.lastAppliedIndex = applyMsg.CommandIndex
		kvs.lastAppliedTerm = applyMsg.CommandTerm
		kvs.mu.Unlock()

		kvs.opMu.Lock()
		if ch, exists := kvs.pendingOps[cmd.ID]; exists {
			select {
			case ch <- result:
			default:
			}
			delete(kvs.pendingOps, cmd.ID)
		}
		kvs.opMu.Unlock()

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

// installSnapshotFromApplyMsg installs a snapshot received via apply channel.
//
// The bytes originate from the leader's snapshotter (ReadSnapshot) and use the
// V2 format (SnapshotData with KVData + Sessions); a legacy V1 payload (a bare
// map[string]string) is still accepted for backward compatibility. Sessions are
// restored so at-most-once duplicate detection keeps working after this node
// installs a snapshot (Raft paper Section 8).
//
// The installed snapshot is also persisted to disk when a snapshotter is
// configured. Raft has already discarded its log up to this index and persisted
// the new snapshot boundary, so failing to persist the state machine here would
// leave a restarted follower with neither data nor log.
func (kvs *KVStore) installSnapshotFromApplyMsg(msg *raft.ApplyMsg) {
	snapshotData, err := parseSnapshotBytes(msg.SnapshotData)
	if err != nil {
		kvs.logger.Printf("Failed to unmarshal snapshot data: %v", err)
		return
	}

	kvs.mu.Lock()
	kvs.data = snapshotData.KVData
	kvs.lastAppliedIndex = msg.SnapshotIndex
	kvs.lastAppliedTerm = msg.SnapshotTerm
	kvs.mu.Unlock()

	if snapshotData.Sessions != nil {
		kvs.sessionMu.Lock()
		kvs.sessions = snapshotData.Sessions
		kvs.sessionMu.Unlock()
	}

	// Persist the received snapshot so a follower restart recovers this state.
	// A nil snapshotter keeps the memory-only behavior for tests.
	if kvs.snapshotter != nil {
		if err := kvs.saveSnapshot(msg.SnapshotIndex, msg.SnapshotTerm); err != nil {
			kvs.logger.Printf("Failed to persist installed snapshot: %v", err)
		}
	}

	kvs.logger.Printf("Installed snapshot: entries=%d, sessions=%d, lastIndex=%d, lastTerm=%d",
		len(snapshotData.KVData), len(snapshotData.Sessions), msg.SnapshotIndex, msg.SnapshotTerm)
}

// parseSnapshotBytes decodes snapshot bytes, preferring the V2 format
// (SnapshotData with KVData/Sessions) and falling back to the legacy V1 format
// (a bare map[string]string). V2 is detected by the presence of the kv_data
// field, mirroring KVSnapshotter.LoadSnapshotV2's discrimination.
func parseSnapshotBytes(data []byte) (*SnapshotData, error) {
	var v2 SnapshotData
	if err := json.Unmarshal(data, &v2); err == nil && v2.KVData != nil {
		return &v2, nil
	}

	var v1 map[string]string
	if err := json.Unmarshal(data, &v1); err != nil {
		return nil, err
	}
	if v1 == nil {
		v1 = make(map[string]string)
	}
	return &SnapshotData{KVData: v1}, nil
}

func (kvs *KVStore) executeCommand(cmd *Command) Result {
	// Check for duplicate/stale requests (Raft paper Section 8)
	if cmd.ClientID != "" {
		if result, isDuplicate := kvs.checkDuplicateRequest(cmd); isDuplicate {
			return result
		}
	}

	result := kvs.applyOperation(cmd)

	// Update session for duplicate detection
	if cmd.ClientID != "" {
		kvs.updateClientSession(cmd.ClientID, cmd.SeqNum, result)
	}

	return result
}

// checkDuplicateRequest checks if a request is a duplicate or stale.
// Returns the cached result and true if the request should not be executed.
func (kvs *KVStore) checkDuplicateRequest(cmd *Command) (Result, bool) {
	kvs.sessionMu.RLock()
	session, exists := kvs.sessions[cmd.ClientID]
	kvs.sessionMu.RUnlock()

	if !exists {
		return Result{}, false
	}

	if cmd.SeqNum < session.LastSeqNum {
		kvs.logger.Printf("Stale request from client %s: seqNum=%d < lastSeqNum=%d",
			cmd.ClientID, cmd.SeqNum, session.LastSeqNum)
		return Result{Err: fmt.Errorf("stale request")}, true
	}

	if cmd.SeqNum == session.LastSeqNum {
		kvs.logger.Printf("Duplicate request from client %s: seqNum=%d (returning cached result)",
			cmd.ClientID, cmd.SeqNum)
		return session.LastResult, true
	}

	return Result{}, false
}

// applyOperation applies a KV operation and returns the result
func (kvs *KVStore) applyOperation(cmd *Command) Result {
	kvs.mu.Lock()
	defer kvs.mu.Unlock()

	switch cmd.Op {
	case OpPut:
		kvs.data[cmd.Key] = cmd.Value
		return Result{}
	case OpGet:
		value, exists := kvs.data[cmd.Key]
		if !exists {
			return Result{Err: fmt.Errorf("key not found")}
		}
		return Result{Value: value}
	case OpDelete:
		delete(kvs.data, cmd.Key)
		return Result{}
	default:
		return Result{Err: fmt.Errorf("unknown operation")}
	}
}

// updateClientSession updates the session state for duplicate detection
func (kvs *KVStore) updateClientSession(clientID string, seqNum int, result Result) {
	kvs.sessionMu.Lock()
	defer kvs.sessionMu.Unlock()

	kvs.sessions[clientID] = &ClientSession{
		LastSeqNum: seqNum,
		LastResult: result,
	}
}

func (kvs *KVStore) Put(key, value string) error {
	return kvs.executeOperation(OpPut, key, value, "", 0)
}

// PutWithSession stores a key-value pair on behalf of an identified client.
// The clientID and seqNum enable at-most-once semantics (Raft paper Section 8):
// a retry that reuses the same seqNum is deduplicated by the state machine. An
// empty clientID disables duplicate detection (equivalent to Put).
func (kvs *KVStore) PutWithSession(key, value, clientID string, seqNum int) error {
	return kvs.executeOperation(OpPut, key, value, clientID, seqNum)
}

func (kvs *KVStore) Get(key string) (string, error) {
	if kvs.raft == nil {
		return "", fmt.Errorf("raft node not initialized")
	}

	// Linearizable read via the ReadIndex protocol (Raft dissertation §6.4):
	//  1. ask raft for a commit index that is safe to read. ReadIndex confirms,
	//     via a fresh quorum of heartbeats, that we are still the leader and
	//     captures the current commit index as the read index;
	//  2. wait until our state machine has applied through that index; and
	//  3. serve the value from local state.
	// Unlike the removed lease optimization this trusts no clock: a leader that
	// has lost its quorum (e.g. is partitioned) fails the read instead of
	// returning a value that a newly elected leader may already have superseded.
	// A non-leader returns raft.ErrNotLeader ("not leader"), which the HTTP layer
	// maps to a 503 leader redirect, preserving the previous client behavior.
	readIndex, err := kvs.raft.ReadIndex()
	if err != nil {
		return "", err
	}
	if err := kvs.waitForApplied(readIndex); err != nil {
		return "", err
	}
	return kvs.getLocal(key)
}

// waitForApplied blocks until the state machine has applied through index, or
// until operationTimeout elapses. It is the local half of a ReadIndex read: the
// captured read index may only be served once our applied index has caught up to
// it (§6.4 step 3).
func (kvs *KVStore) waitForApplied(index int) error {
	kvs.mu.RLock()
	applied := kvs.lastAppliedIndex
	kvs.mu.RUnlock()
	if applied >= index {
		return nil
	}

	deadline := time.NewTimer(operationTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(applyWaitPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			kvs.mu.RLock()
			applied := kvs.lastAppliedIndex
			kvs.mu.RUnlock()
			if applied >= index {
				return nil
			}
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for state machine to apply through index %d", index)
		}
	}
}

// isNoOpCommand reports whether an ApplyMsg command is the leader's no-op marker
// (raft.NoOpCommand). The command reaches the apply loop either as a string
// (in-process MockTransport) or as bytes (decoded RPC), so both are handled.
func isNoOpCommand(command interface{}) bool {
	switch c := command.(type) {
	case string:
		return c == raft.NoOpCommand
	case []byte:
		return string(c) == raft.NoOpCommand
	default:
		return false
	}
}

// getLocal reads directly from local state. It must only be called once a
// ReadIndex has been confirmed and the apply index has caught up to it.
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
	return kvs.executeOperation(OpDelete, key, "", "", 0)
}

// DeleteWithSession deletes a key on behalf of an identified client. See
// PutWithSession for the duplicate-detection semantics of clientID/seqNum.
func (kvs *KVStore) DeleteWithSession(key, clientID string, seqNum int) error {
	return kvs.executeOperation(OpDelete, key, "", clientID, seqNum)
}

func (kvs *KVStore) executeOperation(op Operation, key, value, clientID string, seqNum int) error {
	result := kvs.executeOperationWithResult(op, key, value, clientID, seqNum)
	return result.Err
}

func (kvs *KVStore) executeOperationWithResult(op Operation, key, value, clientID string, seqNum int) Result {
	if kvs.raft == nil {
		return Result{Value: "", Err: fmt.Errorf("raft node not initialized")}
	}

	if !kvs.raft.IsLeader() {
		return Result{Value: "", Err: fmt.Errorf("not leader")}
	}

	opID := fmt.Sprintf("%s-%d", kvs.raft.GetNodeID(), time.Now().UnixNano())
	cmd := Command{
		Op:       op,
		Key:      key,
		Value:    value,
		ID:       opID,
		ClientID: clientID,
		SeqNum:   seqNum,
	}

	cmdBytes, err := json.Marshal(cmd)
	if err != nil {
		return Result{Value: "", Err: err}
	}

	_, _, isLeader := kvs.raft.Start(string(cmdBytes))
	if !isLeader {
		return Result{Value: "", Err: fmt.Errorf("not leader")}
	}

	resultCh := make(chan Result, 1)
	kvs.opMu.Lock()
	kvs.pendingOps[opID] = resultCh
	kvs.opMu.Unlock()

	return kvs.awaitOperationResult(resultCh, opID)
}

// awaitOperationResult blocks until the committed result for opID is delivered
// or the operation times out. A received result corresponds to an entry that
// Raft has already committed and applied; by the Leader Completeness Property
// such an entry is durable, so the result is returned regardless of any later
// leadership or term change.
func (kvs *KVStore) awaitOperationResult(resultCh chan Result, opID string) Result {
	timeout := time.NewTimer(operationTimeout)
	defer timeout.Stop()

	select {
	case result := <-resultCh:
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
