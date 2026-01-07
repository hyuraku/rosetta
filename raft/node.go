package raft

import (
	"context"
	"log"
	"sync"
	"time"
)

type RaftNode struct {
	mu        sync.RWMutex
	state     *RaftState
	transport RPCTransport
	done      chan struct{}
	applyCh   chan ApplyMsg

	logger *log.Logger
}

func NewRaftNode(nodeID string, peers []string, transport RPCTransport, applyCh chan ApplyMsg) *RaftNode {
	return NewRaftNodeWithPersister(nodeID, peers, transport, applyCh, nil)
}

func NewRaftNodeWithPersister(nodeID string, peers []string, transport RPCTransport, applyCh chan ApplyMsg, persister Persister) *RaftNode {
	node := &RaftNode{
		state:     NewRaftStateWithPersister(nodeID, peers, applyCh, persister),
		transport: transport,
		done:      make(chan struct{}),
		applyCh:   applyCh,
		logger:    log.New(log.Writer(), "[RAFT-"+nodeID+"] ", log.LstdFlags),
	}

	go node.run()
	return node
}

func (rn *RaftNode) run() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-rn.done:
			return
		case <-rn.state.ElectionTimer():
			rn.handleElectionTimeout()
		case <-ticker.C:
			rn.handleTick()
		}
	}
}

func (rn *RaftNode) handleElectionTimeout() {
	rn.mu.Lock()
	defer rn.mu.Unlock()

	if rn.state.GetNodeState() != Leader {
		rn.logger.Printf("Election timeout, starting election for term %d", rn.state.GetCurrentTerm()+1)
		rn.state.startElection(rn.transport)
	}
}

func (rn *RaftNode) handleTick() {
	if rn.state.GetNodeState() == Leader {
		rn.state.sendHeartbeats(rn.transport)
	}
}

func (rn *RaftNode) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) error {
	rn.logger.Printf("Received RequestVote from %s for term %d", args.CandidateID, args.Term)
	rn.state.RequestVote(args, reply)
	rn.logger.Printf("RequestVote reply: term=%d, granted=%v", reply.Term, reply.VoteGranted)
	return nil
}

func (rn *RaftNode) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) error {
	rn.logger.Printf("Received AppendEntries from %s for term %d", args.LeaderID, args.Term)
	rn.state.AppendEntries(args, reply)
	rn.logger.Printf("AppendEntries reply: term=%d, success=%v", reply.Term, reply.Success)
	return nil
}

func (rn *RaftNode) Start(command interface{}) (int, int, bool) {
	rn.mu.Lock()
	defer rn.mu.Unlock()

	term, isLeader := rn.state.GetState()
	if !isLeader {
		return -1, term, false
	}

	index := rn.state.AppendLogEntry(command, "command")
	rn.logger.Printf("Started command at index %d, term %d", index, term)

	return index, term, true
}

func (rn *RaftNode) GetState() (int, bool) {
	return rn.state.GetState()
}

func (rn *RaftNode) GetNodeID() string {
	return rn.state.nodeID
}

func (rn *RaftNode) GetLeader() string {
	rn.mu.RLock()
	defer rn.mu.RUnlock()

	if rn.state.GetNodeState() == Leader {
		return rn.state.nodeID
	}

	// Return the tracked current leader
	return rn.state.currentLeader
}

func (rn *RaftNode) Kill() {
	close(rn.done)
}

func (rn *RaftNode) GetLogLength() int {
	return rn.state.GetLastLogIndex()
}

func (rn *RaftNode) IsLeader() bool {
	_, isLeader := rn.state.GetState()
	return isLeader
}

// CanServeReadOnlyQuery returns true if the leader can safely serve
// read-only queries without going through the Raft log.
// This implements the lease-based read optimization from Raft paper Section 8.
func (rn *RaftNode) CanServeReadOnlyQuery() bool {
	return rn.state.CanServeReadOnlyQuery()
}

func (rn *RaftNode) GetRaftState() *RaftState {
	return rn.state
}

func (rn *RaftNode) SetLogger(logger *log.Logger) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	rn.logger = logger
}

// TriggerSnapshot triggers log compaction up to the given index
func (rn *RaftNode) TriggerSnapshot(lastIncludedIndex int) {
	// This is called by the KV store when it has saved a snapshot
	// The actual log truncation will happen asynchronously
	rn.logger.Printf("Snapshot trigger received for index %d", lastIncludedIndex)
	// Note: Actual log truncation is handled in the snapshot.go TakeSnapshot method
}

type MockTransport struct {
	nodes map[string]*RaftNode
	mu    sync.RWMutex
}

func NewMockTransport() *MockTransport {
	return &MockTransport{
		nodes: make(map[string]*RaftNode),
	}
}

func (mt *MockTransport) RegisterNode(nodeID string, node *RaftNode) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	mt.nodes[nodeID] = node
}

func (mt *MockTransport) SendRequestVote(ctx context.Context, target string, args *RequestVoteArgs) (*RequestVoteReply, error) {
	mt.mu.RLock()
	node, exists := mt.nodes[target]
	mt.mu.RUnlock()

	if !exists {
		return nil, context.DeadlineExceeded
	}

	reply := &RequestVoteReply{}
	err := node.RequestVote(args, reply)
	return reply, err
}

func (mt *MockTransport) SendAppendEntries(ctx context.Context, target string, args *AppendEntriesArgs) (*AppendEntriesReply, error) {
	mt.mu.RLock()
	node, exists := mt.nodes[target]
	mt.mu.RUnlock()

	if !exists {
		return nil, context.DeadlineExceeded
	}

	reply := &AppendEntriesReply{}
	err := node.AppendEntries(args, reply)
	return reply, err
}

func (mt *MockTransport) SendInstallSnapshot(ctx context.Context, target string, args *InstallSnapshotArgs) (*InstallSnapshotReply, error) {
	mt.mu.RLock()
	node, exists := mt.nodes[target]
	mt.mu.RUnlock()

	if !exists {
		return nil, context.DeadlineExceeded
	}

	reply := &InstallSnapshotReply{}
	node.state.InstallSnapshot(args, reply)
	return reply, nil
}

func (mt *MockTransport) RemoveNode(nodeID string) {
	mt.mu.Lock()
	defer mt.mu.Unlock()
	delete(mt.nodes, nodeID)
}

func (mt *MockTransport) GetNodes() map[string]*RaftNode {
	mt.mu.RLock()
	defer mt.mu.RUnlock()

	result := make(map[string]*RaftNode)
	for k, v := range mt.nodes {
		result[k] = v
	}
	return result
}
