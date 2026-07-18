package integration

import (
	"fmt"
	"testing"
	"time"

	"rosetta/raft"
)

func TestThreeNodeCluster(t *testing.T) {
	peers := []string{"node1", "node2", "node3"}
	nodes := make(map[string]*raft.RaftNode)
	applyChannels := make(map[string]chan raft.ApplyMsg)
	transport := raft.NewMockTransport()

	for _, nodeID := range peers {
		applyCh := make(chan raft.ApplyMsg, 100)
		applyChannels[nodeID] = applyCh

		node := raft.NewRaftNode(nodeID, peers, transport, applyCh)
		nodes[nodeID] = node
		transport.RegisterNode(nodeID, node)
	}

	time.Sleep(300 * time.Millisecond)

	leaderCount := 0
	var leader *raft.RaftNode

	for _, node := range nodes {
		if node.IsLeader() {
			leaderCount++
			leader = node
		}
	}

	if leaderCount != 1 {
		t.Errorf("Expected exactly 1 leader, got %d", leaderCount)
	}

	if leader != nil {
		index, term, isLeader := leader.Start("test command")
		if !isLeader {
			t.Error("Leader should be able to start commands")
		}
		if index <= 0 {
			t.Errorf("Expected positive index, got %d", index)
		}
		if term <= 0 {
			t.Errorf("Expected positive term, got %d", term)
		}
	}

	for _, node := range nodes {
		node.Kill()
	}
}

func TestFiveNodeCluster(t *testing.T) {
	peers := []string{"node1", "node2", "node3", "node4", "node5"}
	nodes := make(map[string]*raft.RaftNode)
	applyChannels := make(map[string]chan raft.ApplyMsg)
	transport := raft.NewMockTransport()

	for _, nodeID := range peers {
		applyCh := make(chan raft.ApplyMsg, 100)
		applyChannels[nodeID] = applyCh

		node := raft.NewRaftNode(nodeID, peers, transport, applyCh)
		nodes[nodeID] = node
		transport.RegisterNode(nodeID, node)
	}

	time.Sleep(500 * time.Millisecond)

	leaderCount := 0
	var leader *raft.RaftNode

	for _, node := range nodes {
		if node.IsLeader() {
			leaderCount++
			leader = node
		}
	}

	if leaderCount != 1 {
		t.Errorf("Expected exactly 1 leader, got %d", leaderCount)
	}

	commands := []string{"cmd1", "cmd2", "cmd3", "cmd4", "cmd5"}
	if leader != nil {
		for i, cmd := range commands {
			index, term, isLeader := leader.Start(cmd)
			if !isLeader {
				t.Errorf("Leader should be able to start command %d", i)
				break
			}
			if index != i+1 {
				t.Errorf("Expected index %d for command %d, got %d", i+1, i, index)
			}
			if term <= 0 {
				t.Errorf("Expected positive term for command %d, got %d", i, term)
			}
		}
	}

	for _, node := range nodes {
		node.Kill()
	}
}

func TestLeaderElectionAfterFailure(t *testing.T) {
	peers := []string{"node1", "node2", "node3"}
	nodes := make(map[string]*raft.RaftNode)
	applyChannels := make(map[string]chan raft.ApplyMsg)
	transport := raft.NewMockTransport()

	for _, nodeID := range peers {
		applyCh := make(chan raft.ApplyMsg, 100)
		applyChannels[nodeID] = applyCh

		node := raft.NewRaftNode(nodeID, peers, transport, applyCh)
		nodes[nodeID] = node
		transport.RegisterNode(nodeID, node)
	}

	time.Sleep(300 * time.Millisecond)

	var originalLeader *raft.RaftNode
	var originalLeaderID string

	for nodeID, node := range nodes {
		if node.IsLeader() {
			originalLeader = node
			originalLeaderID = nodeID
			break
		}
	}

	if originalLeader == nil {
		t.Fatal("No initial leader found")
	}

	originalLeader.Kill()
	transport.RemoveNode(originalLeaderID)
	delete(nodes, originalLeaderID)

	time.Sleep(500 * time.Millisecond)

	leaderCount := 0
	var newLeader *raft.RaftNode

	for _, node := range nodes {
		if node.IsLeader() {
			leaderCount++
			newLeader = node
		}
	}

	if leaderCount != 1 {
		t.Errorf("Expected exactly 1 new leader after failure, got %d", leaderCount)
	}

	if newLeader != nil {
		index, term, isLeader := newLeader.Start("recovery command")
		if !isLeader {
			t.Error("New leader should be able to start commands")
		}
		if index <= 0 {
			t.Errorf("Expected positive index, got %d", index)
		}
		if term <= 0 {
			t.Errorf("Expected positive term, got %d", term)
		}
	}

	for _, node := range nodes {
		node.Kill()
	}
}

func TestLogReplication(t *testing.T) {
	peers := []string{"node1", "node2", "node3"}
	nodes := make(map[string]*raft.RaftNode)
	applyChannels := make(map[string]chan raft.ApplyMsg)
	transport := raft.NewMockTransport()

	for _, nodeID := range peers {
		applyCh := make(chan raft.ApplyMsg, 100)
		applyChannels[nodeID] = applyCh

		node := raft.NewRaftNode(nodeID, peers, transport, applyCh)
		nodes[nodeID] = node
		transport.RegisterNode(nodeID, node)
	}

	time.Sleep(300 * time.Millisecond)

	var leader *raft.RaftNode
	for _, node := range nodes {
		if node.IsLeader() {
			leader = node
			break
		}
	}

	if leader == nil {
		t.Fatal("No leader found")
	}

	commands := []string{"command1", "command2", "command3"}

	for _, cmd := range commands {
		index, term, isLeader := leader.Start(cmd)
		if !isLeader {
			t.Error("Leader should be able to start commands")
			break
		}
		if index <= 0 || term <= 0 {
			t.Errorf("Invalid index or term: index=%d, term=%d", index, term)
		}
	}

	time.Sleep(200 * time.Millisecond)

	for nodeID, node := range nodes {
		logLength := node.GetLogLength()
		if logLength != len(commands) {
			t.Errorf("Node %s should have %d log entries, got %d",
				nodeID, len(commands), logLength)
		}
	}

	for _, node := range nodes {
		node.Kill()
	}
}

func TestNetworkPartition(t *testing.T) {
	peers := []string{"node1", "node2", "node3", "node4", "node5"}
	nodes := make(map[string]*raft.RaftNode)
	applyChannels := make(map[string]chan raft.ApplyMsg)
	transport := raft.NewMockTransport()

	for _, nodeID := range peers {
		applyCh := make(chan raft.ApplyMsg, 100)
		applyChannels[nodeID] = applyCh

		node := raft.NewRaftNode(nodeID, peers, transport, applyCh)
		nodes[nodeID] = node
		transport.RegisterNode(nodeID, node)
	}

	time.Sleep(300 * time.Millisecond)

	// Verify initial leader election
	if countActiveLeaders(nodes, nil) != 1 {
		t.Fatal("No initial leader found")
	}

	// Partition: nodes 1, 2, 3 in majority, nodes 4, 5 in minority
	minorityNodes := []string{"node4", "node5"}

	// Remove minority nodes from transport
	for _, nodeID := range minorityNodes {
		nodes[nodeID].Kill() // Stop minority nodes completely
		transport.RemoveNode(nodeID)
	}

	// Poll for leader in majority partition with retry logic
	if !pollForSingleLeader(nodes, minorityNodes, 5, 300*time.Millisecond) {
		t.Error("Expected 1 leader in majority partition after 5 attempts")
	}

	// Re-add minority nodes
	for _, nodeID := range minorityNodes {
		transport.RegisterNode(nodeID, nodes[nodeID])
	}

	// Poll for single leader after healing
	if !pollForSingleLeader(nodes, minorityNodes, 3, 200*time.Millisecond) {
		t.Error("Expected 1 leader after partition heals")
	}

	// Clean up: kill only the majority nodes (minority already killed)
	for nodeID, node := range nodes {
		if !contains(minorityNodes, nodeID) {
			node.Kill()
		}
	}
}

// countActiveLeaders counts how many nodes currently report leadership,
// skipping any node IDs present in exclude.
func countActiveLeaders(nodes map[string]*raft.RaftNode, exclude []string) int {
	count := 0
	for nodeID, node := range nodes {
		if contains(exclude, nodeID) {
			continue
		}
		if node.IsLeader() {
			count++
		}
	}
	return count
}

// pollForSingleLeader retries up to attempts times, sleeping between each,
// until exactly one non-excluded node reports leadership.
func pollForSingleLeader(nodes map[string]*raft.RaftNode, exclude []string, attempts int, sleep time.Duration) bool {
	for attempt := 0; attempt < attempts; attempt++ {
		time.Sleep(sleep)
		if countActiveLeaders(nodes, exclude) == 1 {
			return true
		}
	}
	return false
}

func TestConcurrentCommands(t *testing.T) {
	peers := []string{"node1", "node2", "node3"}
	nodes := make(map[string]*raft.RaftNode)
	applyChannels := make(map[string]chan raft.ApplyMsg)
	transport := raft.NewMockTransport()

	for _, nodeID := range peers {
		applyCh := make(chan raft.ApplyMsg, 100)
		applyChannels[nodeID] = applyCh

		node := raft.NewRaftNode(nodeID, peers, transport, applyCh)
		nodes[nodeID] = node
		transport.RegisterNode(nodeID, node)
	}

	time.Sleep(300 * time.Millisecond)

	var leader *raft.RaftNode
	for _, node := range nodes {
		if node.IsLeader() {
			leader = node
			break
		}
	}

	if leader == nil {
		t.Fatal("No leader found")
	}

	numCommands := 10
	done := make(chan bool, numCommands)

	for i := 0; i < numCommands; i++ {
		go func(cmdNum int) {
			cmd := fmt.Sprintf("concurrent-cmd-%d", cmdNum)
			index, term, isLeader := leader.Start(cmd)

			if isLeader && index > 0 && term > 0 {
				done <- true
			} else {
				done <- false
			}
		}(i)
	}

	successCount := 0
	timeout := time.NewTimer(2 * time.Second)
	defer timeout.Stop()

	for i := 0; i < numCommands; i++ {
		select {
		case success := <-done:
			if success {
				successCount++
			}
		case <-timeout.C:
			t.Error("Timeout waiting for concurrent commands")
			break
		}
	}

	if successCount != numCommands {
		t.Errorf("Expected %d successful commands, got %d", numCommands, successCount)
	}

	for _, node := range nodes {
		node.Kill()
	}
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}
