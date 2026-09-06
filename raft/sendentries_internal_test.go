package raft

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// Race regression test for the entries a leader ships (KNOWN_ISSUES.md E2).
//
// replicateToPeer used to re-slice persistent.Log under rs.mu.RLock and hand
// that slice to the transport after releasing the lock. The transport reads it
// (the HTTP one JSON-marshals it) while the follower-side handlers keep writing
// the same backing array — an append that fits in the spare capacity writes
// through it, and the truncations move the header over live entries. Run with
// -race: before the fix this reports a data race between the marshal and
// AppendLogEntry/TruncateLogAfter.

// marshalTransport does to args what the real HTTP transport does: serializes
// it outside the sender's lock.
type marshalTransport struct{}

func (marshalTransport) SendRequestVote(
	_ context.Context, _ string, args *RequestVoteArgs,
) (*RequestVoteReply, error) {
	return &RequestVoteReply{Term: args.Term}, nil
}

func (marshalTransport) SendAppendEntries(
	_ context.Context, _ string, args *AppendEntriesArgs,
) (*AppendEntriesReply, error) {
	if _, err := json.Marshal(args); err != nil {
		return nil, err
	}
	return &AppendEntriesReply{Term: args.Term, Success: true}, nil
}

func (marshalTransport) SendInstallSnapshot(
	_ context.Context, _ string, args *InstallSnapshotArgs,
) (*InstallSnapshotReply, error) {
	return &InstallSnapshotReply{Term: args.Term}, nil
}

func TestReplicatedEntriesAreCopiedBeforeSending(t *testing.T) {
	const (
		sendRounds  = 300
		writeRounds = 300
	)

	applyCh := make(chan ApplyMsg, 64)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range applyCh {
		}
	}()

	const peerID = "n2"
	rs := NewRaftState("n1", []string{"n1", peerID}, applyCh)
	rs.mu.Lock()
	rs.persistent.CurrentTerm = 1
	rs.mu.Unlock()
	rs.SetState(Leader)

	// Seed a few entries so the very first send already carries a suffix.
	for i := 0; i < 4; i++ {
		if _, err := rs.AppendLogEntry("seed", entryTypeCommand); err != nil {
			t.Fatalf("seeding the log: %v", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// The send side. NextIndex is pinned to 1 before each round so prevLogIndex
	// lands exactly on the (zero) snapshot boundary: the whole log goes out every
	// time, which is the widest possible window onto the shared array, and the
	// send never has to read a log position the writer may have truncated away.
	go func() {
		defer wg.Done()
		for i := 0; i < sendRounds; i++ {
			rs.mu.Lock()
			if rs.leader != nil {
				rs.leader.NextIndex[peerID] = 1
			}
			rs.mu.Unlock()
			rs.replicateToPeer(marshalTransport{}, peerID, 1, 0)
		}
	}()

	// The mutating side. Appending and then truncating the same tail position over
	// and over is what actually collides with a send in flight: the truncation
	// only moves the slice header, so the next append rewrites a slot that a
	// sender still has inside its own (longer) slice and is marshaling right now.
	go func() {
		defer wg.Done()
		for i := 0; i < writeRounds; i++ {
			index, err := rs.AppendLogEntry("churn", entryTypeCommand)
			if err != nil {
				t.Errorf("AppendLogEntry: %v", err)
				return
			}
			if err := rs.TruncateLogAfter(index - 1); err != nil {
				t.Errorf("TruncateLogAfter: %v", err)
				return
			}
		}
	}()

	wg.Wait()
	// Stop before closing: the applier is the only sender on applyCh and Stop is
	// what guarantees it has exited (KNOWN_ISSUES.md R19). Closing first raced
	// the commits this test drives and panicked with "send on closed channel"
	// once every dozen or so runs of the package.
	rs.Stop()
	close(applyCh)
	<-drained
}
