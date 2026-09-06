package unit

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"rosetta/raft"
)

const (
	// cmdAfterDemotion is the command a demoted node must refuse to append.
	cmdAfterDemotion = "command-after-demotion"
	// cmdUnderRace is the command appended while a higher-term RPC is waiting.
	cmdUnderRace = "command-under-race"
	// higherTermGap keeps the injected RPC's term well above the leader's.
	higherTermGap = 5
)

// newSingleNodeLeader spins up a one-node cluster (its own majority) over the
// mock transport and waits until it has elected itself leader.
func newSingleNodeLeader(t *testing.T) (*raft.RaftNode, *recordingPersister) {
	t.Helper()

	transport := raft.NewMockTransport()
	applyCh := make(chan raft.ApplyMsg, 16)
	drain(applyCh)
	persister := &recordingPersister{}

	node, err := raft.NewRaftNodeWithPersister(nodeID1, []string{nodeID1}, transport, applyCh, persister)
	if err != nil {
		t.Fatalf("unexpected setup error: %v", err)
	}
	transport.RegisterNode(nodeID1, node)
	t.Cleanup(node.Kill)

	if !pollUntil(2*time.Second, node.IsLeader) {
		t.Fatal("single-node cluster did not elect a leader")
	}
	return node, persister
}

// commandEntries returns every live log entry carrying a client command.
func commandEntries(t *testing.T, node *raft.RaftNode) []raft.LogEntry {
	t.Helper()

	entries := make([]raft.LogEntry, 0)
	for i := 1; i <= node.GetLogLength(); i++ {
		entry := node.GetRaftState().GetLogEntry(i)
		if entry == nil || entry.Command == raft.NoOpCommand {
			continue
		}
		entries = append(entries, *entry)
	}
	return entries
}

// R1: a node that was demoted by a higher-term AppendEntries must not append a
// client command afterwards. Before the fix, Start read the role under one rs.mu
// acquisition and appended under another, so a command could land in the log
// stamped with the *new* term — a command the new leader never issued sitting at
// an (index, term) the new leader owns, which breaks Log Matching (§5.4) and the
// Leader Append-Only property.
func TestStart_RefusesCommandAfterHigherTermDemotion(t *testing.T) {
	node, _ := newSingleNodeLeader(t)

	leaderTerm, _ := node.GetState()
	lengthBefore := node.GetLogLength()

	// A higher-term AppendEntries demotes this node to follower.
	reply := &raft.AppendEntriesReply{}
	node.GetRaftState().AppendEntries(&raft.AppendEntriesArgs{
		Term:         leaderTerm + higherTermGap,
		LeaderID:     nodeID2,
		PrevLogIndex: 0,
		PrevLogTerm:  0,
	}, reply)

	if node.IsLeader() {
		t.Fatal("node is still leader after a higher-term AppendEntries")
	}

	index, term, isLeader, err := node.Start(cmdAfterDemotion)
	if isLeader {
		t.Fatal("Start reported isLeader=true after the node was demoted")
	}
	if err != nil {
		t.Errorf("Start on a follower returned an error: %v", err)
	}
	if index > 0 {
		t.Errorf("Start returned index %d on a follower, want no index", index)
	}
	if term <= leaderTerm {
		t.Errorf("Start reported term %d, want the adopted higher term (> %d)", term, leaderTerm)
	}

	if got := node.GetLogLength(); got != lengthBefore {
		t.Fatalf("log grew from %d to %d after Start on a follower", lengthBefore, got)
	}
	for _, entry := range commandEntries(t, node) {
		if entry.Command == cmdAfterDemotion {
			t.Fatalf("demoted node appended the command at index %d, term %d", entry.Index, entry.Term)
		}
		if entry.Term > leaderTerm {
			t.Fatalf("log holds a command entry from term %d, above the term this node led (%d)", entry.Term, leaderTerm)
		}
	}
}

// R1: the leadership check, the term stamped on the entry and the persist must
// all happen under one rs.mu acquisition. The persister blocks inside the save,
// so Start is holding rs.mu when a higher-term AppendEntries arrives from
// another goroutine; that RPC must not be processed until Start has finished,
// and the entry Start appended must carry the term this node actually led in.
func TestStart_LeadershipCheckAndAppendAreAtomic(t *testing.T) {
	node, persister := newSingleNodeLeader(t)

	leaderTerm, _ := node.GetState()

	var once sync.Once
	inPersist := make(chan struct{})
	release := make(chan struct{})
	persister.setHook(func() {
		once.Do(func() {
			close(inPersist)
			<-release
		})
	})

	started := make(chan startResult, 1)
	go func() {
		index, term, isLeader, err := node.Start(cmdUnderRace)
		started <- startResult{index, term, isLeader, err}
	}()

	awaitSignal(t, inPersist, "Start never reached the persist step")

	// Start is now inside persist with rs.mu held. Fire the higher-term RPC and
	// give it time to block on that lock before letting the persist finish.
	appendDone := make(chan struct{})
	go func() {
		defer close(appendDone)
		node.GetRaftState().AppendEntries(&raft.AppendEntriesArgs{
			Term:         leaderTerm + higherTermGap,
			LeaderID:     nodeID2,
			PrevLogIndex: 0,
			PrevLogTerm:  0,
		}, &raft.AppendEntriesReply{})
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)

	var result startResult
	select {
	case result = <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return")
	}
	awaitSignal(t, appendDone, "AppendEntries did not return")

	if !result.isLeader || result.err != nil {
		t.Fatalf("Start = (index %d, term %d, isLeader %v, err %v), want a successful leader append",
			result.index, result.term, result.isLeader, result.err)
	}
	if result.term != leaderTerm {
		t.Errorf("Start reported term %d, want %d (the term it held while appending)", result.term, leaderTerm)
	}

	// The entry carries the term this node actually led in, not the higher term
	// the queued AppendEntries brings.
	requireEntry(t, node, result.index, leaderTerm, cmdUnderRace)

	// The demotion was applied, but only after Start completed.
	if node.IsLeader() {
		t.Error("node is still leader after the higher-term AppendEntries was processed")
	}

	saved := persister.savedLog()
	if len(saved) < result.index {
		t.Fatalf("persisted log holds %d entries, want at least %d", len(saved), result.index)
	}
	if got := saved[result.index-1]; got.Term != leaderTerm || got.Command != cmdUnderRace {
		t.Errorf("persisted entry %d = %+v, want term %d command %q", result.index, got, leaderTerm, cmdUnderRace)
	}
}

// startResult captures the four values RaftNode.Start returns.
type startResult struct {
	index    int
	term     int
	isLeader bool
	err      error
}

// awaitSignal fails the test if ch is not closed within two seconds.
func awaitSignal(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(msg)
	}
}

// requireEntry asserts that the live log holds exactly the expected entry at index.
func requireEntry(t *testing.T, node *raft.RaftNode, index, wantTerm int, wantCommand string) {
	t.Helper()

	entry := node.GetRaftState().GetLogEntry(index)
	if entry == nil {
		t.Fatalf("entry %d is missing from the log", index)
	}
	if entry.Term != wantTerm {
		t.Fatalf("entry %d was stamped with term %d, want %d: a demotion interleaved with the append",
			index, entry.Term, wantTerm)
	}
	if entry.Command != wantCommand {
		t.Fatalf("entry %d holds %v, want %q", index, entry.Command, wantCommand)
	}
}

// R1, stress: hammer Start while higher-term AppendEntries keep demoting the
// node, then check every command that Start accepted.
//
// Start's contract is that the term it returns is the term it appended under.
// While the role check and the append lived in separate rs.mu critical sections,
// a demotion could land between them: Start returned the old (leader) term while
// the entry went into the log stamped with the new one — a command entry written
// by a non-leader, at an (index, term) the new leader owns. Run under -race this
// also covers the unsynchronized read of the role.
func TestStart_StressAgainstConcurrentDemotion(t *testing.T) {
	node, _ := newSingleNodeLeader(t)

	baseTerm, _ := node.GetState()

	type accepted struct {
		index   int
		term    int
		command string
	}
	var (
		mu      sync.Mutex
		results []accepted
	)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			command := stressCommand(i)
			index, term, isLeader, err := node.Start(command)
			if !isLeader || err != nil {
				continue
			}
			mu.Lock()
			results = append(results, accepted{index, term, command})
			mu.Unlock()
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for term := baseTerm + 1; ; term++ {
			select {
			case <-stop:
				return
			default:
			}
			// Put the node back in the leader role, then demote it again with a
			// higher term. Flipping this fast is artificial, but it is the
			// cheapest way to keep hitting the window Start has to close: the
			// role still reads Leader when Start looks at it, and the term has
			// moved on by the time the entry would be stamped.
			node.GetRaftState().SetState(raft.Leader)
			node.GetRaftState().AppendEntries(&raft.AppendEntriesArgs{
				Term:         term,
				LeaderID:     nodeID2,
				PrevLogIndex: 0,
				PrevLogTerm:  0,
			}, &raft.AppendEntriesReply{})
		}
	}()

	time.Sleep(500 * time.Millisecond)
	close(stop)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(results) == 0 {
		t.Fatal("no command was accepted during the stress run")
	}
	for _, got := range results {
		entry := node.GetRaftState().GetLogEntry(got.index)
		if entry == nil {
			t.Fatalf("Start accepted %q at index %d but the entry is gone", got.command, got.index)
		}
		if entry.Command != got.command {
			t.Fatalf("index %d holds %v, but Start reported it accepted %q", got.index, entry.Command, got.command)
		}
		if entry.Term != got.term {
			t.Fatalf("Start returned term %d for index %d but the entry was stamped term %d: "+
				"a demotion interleaved with the append", got.term, got.index, entry.Term)
		}
	}
}

func stressCommand(i int) string {
	return "stress-" + strconv.Itoa(i)
}
