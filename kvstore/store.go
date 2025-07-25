package kvstore

import (
	"encoding/json"
	"fmt"
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
}

type Result struct {
	Value string `json:"value"`
	Err   error  `json:"error"`
}

type KVStore struct {
	mu      sync.RWMutex
	data    map[string]string
	raft    *raft.RaftNode
	applyCh chan raft.ApplyMsg

	pendingOps map[string]chan Result
	opMu       sync.RWMutex

	maxRaftState int
}

func NewKVStore(maxRaftState int) *KVStore {
	applyCh := make(chan raft.ApplyMsg, 100)

	kvs := &KVStore{
		data:         make(map[string]string),
		applyCh:      applyCh,
		pendingOps:   make(map[string]chan Result),
		maxRaftState: maxRaftState,
	}

	go kvs.applyLoop()
	return kvs
}

func (kvs *KVStore) SetRaft(raftNode *raft.RaftNode) {
	kvs.raft = raftNode
}

func (kvs *KVStore) applyLoop() {
	for applyMsg := range kvs.applyCh {
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

		kvs.opMu.RLock()
		if ch, exists := kvs.pendingOps[cmd.ID]; exists {
			select {
			case ch <- result:
			default:
			}
			delete(kvs.pendingOps, cmd.ID)
		}
		kvs.opMu.RUnlock()
	}
}

func (kvs *KVStore) executeCommand(cmd *Command) Result {
	kvs.mu.Lock()
	defer kvs.mu.Unlock()

	switch cmd.Op {
	case OpPut:
		kvs.data[cmd.Key] = cmd.Value
		return Result{Value: "", Err: nil}
	case OpGet:
		value, exists := kvs.data[cmd.Key]
		if !exists {
			return Result{Value: "", Err: fmt.Errorf("key not found")}
		}
		return Result{Value: value, Err: nil}
	case OpDelete:
		delete(kvs.data, cmd.Key)
		return Result{Value: "", Err: nil}
	default:
		return Result{Value: "", Err: fmt.Errorf("unknown operation")}
	}
}

func (kvs *KVStore) Put(key, value string) error {
	return kvs.executeOperation(OpPut, key, value)
}

func (kvs *KVStore) Get(key string) (string, error) {
	result := kvs.executeOperationWithResult(OpGet, key, "")
	if result.Err != nil {
		return "", result.Err
	}
	return result.Value, nil
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
