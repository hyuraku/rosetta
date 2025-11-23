package raft

import (
	"log"
	"math/rand"
	"sync"
	"time"
)

type NodeState int

const (
	Follower NodeState = iota
	Candidate
	Leader
)

func (s NodeState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	default:
		return "Unknown"
	}
}

type PersistentState struct {
	CurrentTerm int
	VotedFor    *string
	Log         []LogEntry

	// Snapshot metadata
	LastIncludedIndex int // Index of last entry in snapshot
	LastIncludedTerm  int // Term of last entry in snapshot
}

type VolatileState struct {
	CommitIndex int
	LastApplied int
}

type LeaderState struct {
	NextIndex  map[string]int
	MatchIndex map[string]int
}

// Persister interface for saving/loading persistent state
type Persister interface {
	SaveRaftState(state *PersistentState) error
	LoadRaftState() (*PersistentState, error)
}

type RaftState struct {
	mu sync.RWMutex

	nodeID string
	state  NodeState
	peers  []string

	persistent PersistentState
	volatile   VolatileState
	leader     *LeaderState

	currentLeader string // Track the current leader ID

	electionTimeout  time.Duration
	heartbeatTimeout time.Duration
	lastHeartbeat    time.Time
	electionTimer    *time.Timer

	applyCh   chan ApplyMsg
	persister Persister
	logger    *log.Logger
}

type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int

	// For snapshots
	SnapshotValid bool
	SnapshotIndex int
	SnapshotTerm  int
	SnapshotData  []byte
}

func NewRaftState(nodeID string, peers []string, applyCh chan ApplyMsg) *RaftState {
	return NewRaftStateWithPersister(nodeID, peers, applyCh, nil)
}

func NewRaftStateWithPersister(nodeID string, peers []string, applyCh chan ApplyMsg, persister Persister) *RaftState {
	// Randomized election timeout between 150ms and 300ms
	// This prevents split votes when nodes start at the same time
	randomTimeout := 150 + rand.Intn(150)

	rs := &RaftState{
		nodeID:           nodeID,
		state:            Follower,
		peers:            peers,
		persistent:       PersistentState{CurrentTerm: 0, VotedFor: nil, Log: make([]LogEntry, 0)},
		volatile:         VolatileState{CommitIndex: 0, LastApplied: 0},
		electionTimeout:  time.Duration(randomTimeout) * time.Millisecond,
		heartbeatTimeout: 50 * time.Millisecond,
		lastHeartbeat:    time.Now(),
		applyCh:          applyCh,
		persister:        persister,
		logger:           log.New(log.Writer(), "[RAFT-STATE-"+nodeID+"] ", log.LstdFlags),
	}

	// Load persistent state if persister is available
	if persister != nil {
		if err := rs.loadPersistentState(); err != nil {
			rs.logger.Printf("Warning: failed to load persistent state: %v", err)
		}
	}

	rs.electionTimer = time.NewTimer(rs.electionTimeout)

	return rs
}

// loadPersistentState loads the persistent state from storage
func (rs *RaftState) loadPersistentState() error {
	state, err := rs.persister.LoadRaftState()
	if err != nil {
		return err
	}

	if state != nil {
		rs.persistent = *state
		rs.logger.Printf("Loaded persistent state: term=%d, log_size=%d", state.CurrentTerm, len(state.Log))
	}

	return nil
}

// persist saves the current persistent state to storage
func (rs *RaftState) persist() {
	if rs.persister == nil {
		return
	}

	if err := rs.persister.SaveRaftState(&rs.persistent); err != nil {
		rs.logger.Printf("Error: failed to persist state: %v", err)
	}
}

func (rs *RaftState) GetState() (int, bool) {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.persistent.CurrentTerm, rs.state == Leader
}

func (rs *RaftState) GetCurrentTerm() int {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.persistent.CurrentTerm
}

func (rs *RaftState) GetNodeState() NodeState {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.state
}

func (rs *RaftState) SetState(state NodeState) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.state = state

	if state == Leader {
		rs.initializeLeaderState()
	} else {
		rs.leader = nil
	}
}

func (rs *RaftState) IncrementTerm() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.persistent.CurrentTerm++
	rs.persistent.VotedFor = nil
	rs.persist()
}

func (rs *RaftState) SetVotedFor(nodeID *string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.persistent.VotedFor = nodeID
	rs.persist()
}

func (rs *RaftState) GetVotedFor() *string {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.persistent.VotedFor
}

func (rs *RaftState) ResetElectionTimer() {
	// Randomize election timeout on each reset to prevent split votes
	randomTimeout := 150 + rand.Intn(150)
	rs.electionTimeout = time.Duration(randomTimeout) * time.Millisecond
	rs.electionTimer.Reset(rs.electionTimeout)
	rs.lastHeartbeat = time.Now()
}

func (rs *RaftState) ElectionTimer() <-chan time.Time {
	return rs.electionTimer.C
}

func (rs *RaftState) initializeLeaderState() {
	rs.leader = &LeaderState{
		NextIndex:  make(map[string]int),
		MatchIndex: make(map[string]int),
	}

	nextIndex := len(rs.persistent.Log) + 1
	for _, peer := range rs.peers {
		if peer != rs.nodeID {
			rs.leader.NextIndex[peer] = nextIndex
			rs.leader.MatchIndex[peer] = 0
		}
	}
}

func (rs *RaftState) GetLeaderState() *LeaderState {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.leader
}

func (rs *RaftState) UpdateLastHeartbeat() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.lastHeartbeat = time.Now()
}

func (rs *RaftState) GetLastHeartbeat() time.Time {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.lastHeartbeat
}
