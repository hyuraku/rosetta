package raft

import (
	"context"
	"encoding/json"
	"time"
)

type RequestVoteArgs struct {
	Term         int    `json:"term"`
	CandidateID  string `json:"candidateId"`
	LastLogIndex int    `json:"lastLogIndex"`
	LastLogTerm  int    `json:"lastLogTerm"`
}

type RequestVoteReply struct {
	Term        int  `json:"term"`
	VoteGranted bool `json:"voteGranted"`
}

type AppendEntriesArgs struct {
	Term         int        `json:"term"`
	LeaderID     string     `json:"leaderId"`
	PrevLogIndex int        `json:"prevLogIndex"`
	PrevLogTerm  int        `json:"prevLogTerm"`
	Entries      []LogEntry `json:"entries"`
	LeaderCommit int        `json:"leaderCommit"`
}

type AppendEntriesReply struct {
	Term    int  `json:"term"`
	Success bool `json:"success"`
}

type InstallSnapshotArgs struct {
	Term              int    `json:"term"`
	LeaderID          string `json:"leaderId"`
	LastIncludedIndex int    `json:"lastIncludedIndex"`
	LastIncludedTerm  int    `json:"lastIncludedTerm"`
	Data              []byte `json:"data"`
}

type InstallSnapshotReply struct {
	Term int `json:"term"`
}

type RPCTransport interface {
	SendRequestVote(ctx context.Context, target string, args *RequestVoteArgs) (*RequestVoteReply, error)
	SendAppendEntries(ctx context.Context, target string, args *AppendEntriesArgs) (*AppendEntriesReply, error)
	SendInstallSnapshot(ctx context.Context, target string, args *InstallSnapshotArgs) (*InstallSnapshotReply, error)
}

func (rs *RaftState) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	reply.Term = rs.persistent.CurrentTerm
	reply.VoteGranted = false

	if args.Term < rs.persistent.CurrentTerm {
		return
	}

	if args.Term > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = args.Term
		rs.persistent.VotedFor = nil
		rs.state = Follower
	}

	if rs.persistent.VotedFor == nil || *rs.persistent.VotedFor == args.CandidateID {
		lastLogIndex := len(rs.persistent.Log)
		lastLogTerm := 0
		if lastLogIndex > 0 {
			lastLogTerm = rs.persistent.Log[lastLogIndex-1].Term
		}

		if args.LastLogTerm > lastLogTerm ||
			(args.LastLogTerm == lastLogTerm && args.LastLogIndex >= lastLogIndex) {
			rs.persistent.VotedFor = &args.CandidateID
			reply.VoteGranted = true
			rs.ResetElectionTimer()
		}
	}

	reply.Term = rs.persistent.CurrentTerm
}

func (rs *RaftState) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	reply.Term = rs.persistent.CurrentTerm
	reply.Success = false

	if args.Term < rs.persistent.CurrentTerm {
		return
	}

	if args.Term > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = args.Term
		rs.persistent.VotedFor = nil
	}

	rs.state = Follower
	rs.currentLeader = args.LeaderID // Track who the current leader is
	rs.ResetElectionTimer()

	if args.PrevLogIndex > len(rs.persistent.Log) {
		return
	}

	if args.PrevLogIndex > 0 && rs.persistent.Log[args.PrevLogIndex-1].Term != args.PrevLogTerm {
		return
	}

	if len(args.Entries) > 0 {
		if args.PrevLogIndex < len(rs.persistent.Log) {
			rs.persistent.Log = rs.persistent.Log[:args.PrevLogIndex]
		}
		rs.persistent.Log = append(rs.persistent.Log, args.Entries...)

		for i := range rs.persistent.Log[args.PrevLogIndex:] {
			rs.persistent.Log[args.PrevLogIndex+i].Index = args.PrevLogIndex + i + 1
		}
		rs.persist()
	}

	if args.LeaderCommit > rs.volatile.CommitIndex {
		rs.volatile.CommitIndex = min(args.LeaderCommit, len(rs.persistent.Log))
		rs.applyEntries()
	}

	reply.Success = true
	reply.Term = rs.persistent.CurrentTerm
}

func (rs *RaftState) startElection(transport RPCTransport) {
	rs.mu.Lock()
	rs.persistent.CurrentTerm++
	rs.state = Candidate
	rs.persistent.VotedFor = &rs.nodeID
	rs.currentLeader = "" // Clear current leader when starting election
	currentTerm := rs.persistent.CurrentTerm
	lastLogIndex := len(rs.persistent.Log)
	lastLogTerm := 0
	if lastLogIndex > 0 {
		lastLogTerm = rs.persistent.Log[lastLogIndex-1].Term
	}
	rs.mu.Unlock()

	rs.ResetElectionTimer()

	votes := 1
	votesNeeded := len(rs.peers)/2 + 1

	// If this is a single-node cluster, immediately become leader
	if len(rs.peers) == 1 {
		rs.mu.Lock()
		rs.state = Leader
		rs.currentLeader = rs.nodeID // Set self as leader
		rs.initializeLeaderState()
		rs.mu.Unlock()
		// Stop election timer for leader
		rs.electionTimer.Stop()
		return
	}

	for _, peer := range rs.peers {
		if peer == rs.nodeID {
			continue
		}

		go func(peerID string) {
			args := &RequestVoteArgs{
				Term:         currentTerm,
				CandidateID:  rs.nodeID,
				LastLogIndex: lastLogIndex,
				LastLogTerm:  lastLogTerm,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			reply, err := transport.SendRequestVote(ctx, peerID, args)
			if err != nil {
				return
			}

			rs.mu.Lock()
			defer rs.mu.Unlock()

			if rs.persistent.CurrentTerm != currentTerm || rs.state != Candidate {
				return
			}

			if reply.Term > rs.persistent.CurrentTerm {
				rs.persistent.CurrentTerm = reply.Term
				rs.state = Follower
				rs.persistent.VotedFor = nil
				return
			}

			if reply.VoteGranted {
				votes++
				if votes >= votesNeeded && rs.state == Candidate {
					rs.state = Leader
					rs.currentLeader = rs.nodeID // Set self as leader
					rs.initializeLeaderState()
					// Stop election timer for leader
					rs.electionTimer.Stop()
				}
			}
		}(peer)
	}
}

func (rs *RaftState) sendHeartbeats(transport RPCTransport) {
	if rs.GetNodeState() != Leader {
		return
	}

	rs.mu.RLock()
	currentTerm := rs.persistent.CurrentTerm
	commitIndex := rs.volatile.CommitIndex
	isSingleNode := len(rs.peers) == 1
	rs.mu.RUnlock()

	// For single-node cluster, immediately commit any uncommitted entries
	if isSingleNode {
		rs.mu.Lock()
		rs.updateCommitIndex()
		rs.mu.Unlock()
		return
	}

	for _, peer := range rs.peers {
		if peer == rs.nodeID {
			continue
		}

		go func(peerID string) {
			rs.mu.RLock()
			nextIndex := rs.leader.NextIndex[peerID]
			prevLogIndex := nextIndex - 1
			prevLogTerm := 0
			if prevLogIndex > 0 {
				prevLogTerm = rs.persistent.Log[prevLogIndex-1].Term
			}

			entries := make([]LogEntry, 0)
			if nextIndex <= len(rs.persistent.Log) {
				entries = rs.persistent.Log[nextIndex-1:]
			}
			rs.mu.RUnlock()

			args := &AppendEntriesArgs{
				Term:         currentTerm,
				LeaderID:     rs.nodeID,
				PrevLogIndex: prevLogIndex,
				PrevLogTerm:  prevLogTerm,
				Entries:      entries,
				LeaderCommit: commitIndex,
			}

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()

			reply, err := transport.SendAppendEntries(ctx, peerID, args)
			if err != nil {
				return
			}

			rs.mu.Lock()
			defer rs.mu.Unlock()

			if reply.Term > rs.persistent.CurrentTerm {
				rs.persistent.CurrentTerm = reply.Term
				rs.state = Follower
				rs.persistent.VotedFor = nil
				return
			}

			if rs.state != Leader || rs.persistent.CurrentTerm != currentTerm {
				return
			}

			if reply.Success {
				rs.leader.MatchIndex[peerID] = prevLogIndex + len(entries)
				rs.leader.NextIndex[peerID] = rs.leader.MatchIndex[peerID] + 1
				rs.updateCommitIndex()
			} else {
				if rs.leader.NextIndex[peerID] > 1 {
					rs.leader.NextIndex[peerID]--
				}
			}
		}(peer)
	}
}

func (rs *RaftState) updateCommitIndex() {
	if rs.state != Leader {
		return
	}

	for n := rs.volatile.CommitIndex + 1; n <= len(rs.persistent.Log); n++ {
		if rs.persistent.Log[n-1].Term != rs.persistent.CurrentTerm {
			continue
		}

		count := 1
		for _, peer := range rs.peers {
			if peer != rs.nodeID && rs.leader.MatchIndex[peer] >= n {
				count++
			}
		}

		if count*2 > len(rs.peers) {
			rs.volatile.CommitIndex = n
			rs.applyEntries()
		}
	}
}

func SerializeRequestVote(args *RequestVoteArgs) ([]byte, error) {
	return json.Marshal(args)
}

func DeserializeRequestVote(data []byte) (*RequestVoteArgs, error) {
	var args RequestVoteArgs
	err := json.Unmarshal(data, &args)
	return &args, err
}

func SerializeRequestVoteReply(reply *RequestVoteReply) ([]byte, error) {
	return json.Marshal(reply)
}

func DeserializeRequestVoteReply(data []byte) (*RequestVoteReply, error) {
	var reply RequestVoteReply
	err := json.Unmarshal(data, &reply)
	return &reply, err
}

func SerializeAppendEntries(args *AppendEntriesArgs) ([]byte, error) {
	return json.Marshal(args)
}

func DeserializeAppendEntries(data []byte) (*AppendEntriesArgs, error) {
	var args AppendEntriesArgs
	err := json.Unmarshal(data, &args)
	return &args, err
}

func SerializeAppendEntriesReply(reply *AppendEntriesReply) ([]byte, error) {
	return json.Marshal(reply)
}

func DeserializeAppendEntriesReply(data []byte) (*AppendEntriesReply, error) {
	var reply AppendEntriesReply
	err := json.Unmarshal(data, &reply)
	return &reply, err
}

// InstallSnapshot RPC handler
func (rs *RaftState) InstallSnapshot(args *InstallSnapshotArgs, reply *InstallSnapshotReply) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	// Reply immediately if term is stale
	if args.Term < rs.persistent.CurrentTerm {
		reply.Term = rs.persistent.CurrentTerm
		return
	}

	// Update term if necessary
	if args.Term > rs.persistent.CurrentTerm {
		rs.persistent.CurrentTerm = args.Term
		rs.persistent.VotedFor = nil
		rs.state = Follower
		rs.persist()
	}

	reply.Term = rs.persistent.CurrentTerm

	// Reset election timer - valid communication from leader
	rs.state = Follower
	rs.currentLeader = args.LeaderID
	rs.ResetElectionTimer()

	// Don't install older snapshots
	if args.LastIncludedIndex <= rs.persistent.LastIncludedIndex {
		return
	}

	//  Discard log entries covered by snapshot
	newLog := make([]LogEntry, 0)
	for _, entry := range rs.persistent.Log {
		if entry.Index > args.LastIncludedIndex {
			newLog = append(newLog, entry)
		}
	}
	rs.persistent.Log = newLog

	// Update snapshot metadata
	rs.persistent.LastIncludedIndex = args.LastIncludedIndex
	rs.persistent.LastIncludedTerm = args.LastIncludedTerm

	// Update commit index and last applied
	if rs.volatile.CommitIndex < args.LastIncludedIndex {
		rs.volatile.CommitIndex = args.LastIncludedIndex
	}
	if rs.volatile.LastApplied < args.LastIncludedIndex {
		rs.volatile.LastApplied = args.LastIncludedIndex
	}

	// Persist state
	rs.persist()

	// Send snapshot data to apply channel for state machine to install
	rs.applyCh <- ApplyMsg{
		CommandValid:  false,
		Command:       args.Data,
		CommandIndex:  args.LastIncludedIndex,
		SnapshotValid: true,
		SnapshotIndex: args.LastIncludedIndex,
		SnapshotTerm:  args.LastIncludedTerm,
		SnapshotData:  args.Data,
	}
}

// Serialization for InstallSnapshot
func SerializeInstallSnapshotArgs(args *InstallSnapshotArgs) ([]byte, error) {
	return json.Marshal(args)
}

func DeserializeInstallSnapshotArgs(data []byte) (*InstallSnapshotArgs, error) {
	var args InstallSnapshotArgs
	err := json.Unmarshal(data, &args)
	return &args, err
}

func SerializeInstallSnapshotReply(reply *InstallSnapshotReply) ([]byte, error) {
	return json.Marshal(reply)
}

func DeserializeInstallSnapshotReply(data []byte) (*InstallSnapshotReply, error) {
	var reply InstallSnapshotReply
	err := json.Unmarshal(data, &reply)
	return &reply, err
}
