package raft

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// killWithin fails the test if Kill does not return, which is what a missing
// join or a goroutine that ignores the stop signal looks like.
func killWithin(t *testing.T, node *RaftNode, limit time.Duration) {
	t.Helper()

	killed := make(chan struct{})
	go func() {
		defer close(killed)
		node.Kill()
	}()
	select {
	case <-killed:
	case <-time.After(limit):
		t.Fatalf("Kill did not return within %s", limit)
	}
}

// TestKillLeavesNoSenderOnApplyCh is the R19 regression test. It reproduces the
// exact sequence every caller uses — Kill, then close the apply channel — with a
// state machine that has stopped reading, so a delivery is guaranteed to be in
// progress at the moment Kill is called.
//
// Before Kill learned to wait, it only closed done and returned. The tick handler
// was still inside applyEntries, blocked on a send, and closing applyCh on the
// next line killed the process with "send on closed channel" — the flake that
// made TestFullSystemPersistence_CrashAndRecover fail intermittently in CI.
func TestKillLeavesNoSenderOnApplyCh(t *testing.T) {
	// Unbuffered: once the drainer stops, the very next delivery blocks.
	applyCh := make(chan ApplyMsg)
	delivered := make(chan ApplyMsg, 64)
	stopDrain := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-stopDrain:
				return
			case msg := <-applyCh:
				delivered <- msg
			}
		}
	}()

	node := NewRaftNode("n1", []string{"n1"}, NewMockTransport(), applyCh)

	// A single-node cluster elects itself and commits its no-op; seeing it
	// applied means the node is leading and the commit path is live.
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("node never applied its no-op")
	}

	// Queue a backlog while the state machine is still reading.
	for i := 0; i < 8; i++ {
		if _, _, isLeader, err := node.Start(fmt.Sprintf("cmd-%d", i)); err != nil || !isLeader {
			t.Fatalf("Start(%d): isLeader=%v err=%v", i, isLeader, err)
		}
	}

	// Now stall the state machine and let the heartbeat ticks commit the
	// backlog, so a delivery is parked on the channel.
	close(stopDrain)
	<-drainDone
	time.Sleep(300 * time.Millisecond)

	killWithin(t, node, 5*time.Second)

	// Safe only because Kill joined the applier: it is the sole sender.
	close(applyCh)
	// A late send would panic here rather than in some later, unrelated test.
	time.Sleep(300 * time.Millisecond)
}

// TestKillUnderLoadIsClean drives writes at a three-node cluster and shuts it
// down mid-flight, in the Kill-then-Close order the callers use. It is the
// -race/-count regression for the shutdown path: no panic, no deadlock, and
// every Kill returns.
func TestKillUnderLoadIsClean(t *testing.T) {
	transport := NewMockTransport()
	peers := []string{"n1", "n2", "n3"}

	nodes := make([]*RaftNode, 0, len(peers))
	channels := make([]chan ApplyMsg, 0, len(peers))
	var drainers sync.WaitGroup
	stopDrain := make(chan struct{})
	for _, id := range peers {
		applyCh := make(chan ApplyMsg, 4)
		channels = append(channels, applyCh)
		drainers.Add(1)
		go func() {
			defer drainers.Done()
			for {
				select {
				case <-stopDrain:
					return
				case <-applyCh:
				}
			}
		}()
		node := NewRaftNode(id, peers, transport, applyCh)
		transport.RegisterNode(id, node)
		nodes = append(nodes, node)
	}

	// Hammer Start on every node. Only the leader accepts, which is the point:
	// the followers exercise the reject path at the same time.
	stopLoad := make(chan struct{})
	var load sync.WaitGroup
	for _, node := range nodes {
		load.Add(1)
		go func() {
			defer load.Done()
			for i := 0; ; i++ {
				select {
				case <-stopLoad:
					return
				default:
				}
				_, _, _, _ = node.Start(fmt.Sprintf("%s-%d", node.GetNodeID(), i))
				time.Sleep(time.Millisecond)
			}
		}()
	}

	time.Sleep(700 * time.Millisecond)
	close(stopLoad)
	load.Wait()

	for _, node := range nodes {
		killWithin(t, node, 10*time.Second)
	}
	// The state machines shut down after Raft, exactly as main.go does it.
	close(stopDrain)
	drainers.Wait()
	for _, ch := range channels {
		close(ch)
	}
	time.Sleep(200 * time.Millisecond)
}
