package raft

import (
	"testing"
	"time"
)

// White-box tests for wiring config.Config's ElectionTimeout/HeartbeatTimeout
// into raft via the WithTiming option (KNOWN_ISSUES.md R16). None of these
// wait out an actual election or heartbeat — they only check that the fields
// a RaftState computes from Timing land in the documented range.

func newTimingTestState(t *testing.T, opts ...Option) *RaftState {
	t.Helper()
	applyCh := make(chan ApplyMsg, 1)
	rs, err := NewRaftStateWithPersister("n1", []string{"n2", "n3"}, applyCh, nil, opts...)
	if err != nil {
		t.Fatalf("NewRaftStateWithPersister: %v", err)
	}
	t.Cleanup(rs.Stop)
	return rs
}

// TestWithTiming_ElectionTimeoutInRange checks that a custom Timing's election
// timeout lands in [base, base+jitter) at construction, and that the
// heartbeat interval is exactly what was requested.
func TestWithTiming_ElectionTimeoutInRange(t *testing.T) {
	timing := Timing{
		ElectionTimeoutBase:   200 * time.Millisecond,
		ElectionTimeoutJitter: 50 * time.Millisecond,
		HeartbeatInterval:     20 * time.Millisecond,
	}
	rs := newTimingTestState(t, WithTiming(timing))

	rs.mu.RLock()
	got := rs.electionTimeout
	rs.mu.RUnlock()

	if got < timing.ElectionTimeoutBase || got >= timing.ElectionTimeoutBase+timing.ElectionTimeoutJitter {
		t.Errorf("electionTimeout = %v, want in [%v, %v)", got, timing.ElectionTimeoutBase,
			timing.ElectionTimeoutBase+timing.ElectionTimeoutJitter)
	}
	if hb := rs.HeartbeatInterval(); hb != timing.HeartbeatInterval {
		t.Errorf("HeartbeatInterval() = %v, want %v", hb, timing.HeartbeatInterval)
	}
}

// TestWithTiming_ZeroValueNormalizesToDefaults checks that a zero-valued
// Timing (e.g. what an unpopulated config.Config would produce before R16's
// wiring in main.go) is filled in with DefaultTiming's values rather than
// producing an unusable (or panicking, for the jitter) RaftState.
func TestWithTiming_ZeroValueNormalizesToDefaults(t *testing.T) {
	rs := newTimingTestState(t, WithTiming(Timing{}))

	rs.mu.RLock()
	got := rs.electionTimeout
	rs.mu.RUnlock()

	d := DefaultTiming()
	if got < d.ElectionTimeoutBase || got >= d.ElectionTimeoutBase+d.ElectionTimeoutJitter {
		t.Errorf("electionTimeout = %v, want in [%v, %v) (DefaultTiming)", got, d.ElectionTimeoutBase,
			d.ElectionTimeoutBase+d.ElectionTimeoutJitter)
	}
	if hb := rs.HeartbeatInterval(); hb != d.HeartbeatInterval {
		t.Errorf("HeartbeatInterval() = %v, want default %v", hb, d.HeartbeatInterval)
	}
}

// TestWithTiming_PartialOverrideKeepsOtherDefaults checks that setting only
// one Timing field leaves the others at DefaultTiming's values, not zero.
func TestWithTiming_PartialOverrideKeepsOtherDefaults(t *testing.T) {
	rs := newTimingTestState(t, WithTiming(Timing{HeartbeatInterval: 10 * time.Millisecond}))

	d := DefaultTiming()
	rs.mu.RLock()
	got := rs.electionTimeout
	rs.mu.RUnlock()

	if got < d.ElectionTimeoutBase || got >= d.ElectionTimeoutBase+d.ElectionTimeoutJitter {
		t.Errorf("electionTimeout = %v, want in [%v, %v) (untouched field falls back to DefaultTiming)",
			got, d.ElectionTimeoutBase, d.ElectionTimeoutBase+d.ElectionTimeoutJitter)
	}
	if hb := rs.HeartbeatInterval(); hb != 10*time.Millisecond {
		t.Errorf("HeartbeatInterval() = %v, want the overridden 10ms", hb)
	}
}

// TestNoOptions_DefaultsUnchanged is a regression guard for the many existing
// NewRaftStateWithPersister/NewRaftNodeWithPersister callers that pass no
// options at all: they must keep getting exactly the pre-R16 150-300ms/50ms
// timing.
func TestNoOptions_DefaultsUnchanged(t *testing.T) {
	rs := newTimingTestState(t)

	d := DefaultTiming()
	rs.mu.RLock()
	got := rs.electionTimeout
	rs.mu.RUnlock()

	if got < d.ElectionTimeoutBase || got >= d.ElectionTimeoutBase+d.ElectionTimeoutJitter {
		t.Errorf("electionTimeout = %v, want in [%v, %v)", got, d.ElectionTimeoutBase,
			d.ElectionTimeoutBase+d.ElectionTimeoutJitter)
	}
	if hb := rs.HeartbeatInterval(); hb != d.HeartbeatInterval {
		t.Errorf("HeartbeatInterval() = %v, want default %v", hb, d.HeartbeatInterval)
	}
}

// TestResetElectionTimerLocked_StaysInRange checks that repeated resets (the
// path every real election-timeout/heartbeat cycle drives) keep landing in
// the configured range, not just the one election timeout picked at
// construction.
func TestResetElectionTimerLocked_StaysInRange(t *testing.T) {
	timing := Timing{
		ElectionTimeoutBase:   30 * time.Millisecond,
		ElectionTimeoutJitter: 10 * time.Millisecond,
		HeartbeatInterval:     5 * time.Millisecond,
	}
	rs := newTimingTestState(t, WithTiming(timing))

	const resets = 50
	for i := 0; i < resets; i++ {
		rs.mu.Lock()
		rs.resetElectionTimerLocked()
		got := rs.electionTimeout
		rs.mu.Unlock()

		if got < timing.ElectionTimeoutBase || got >= timing.ElectionTimeoutBase+timing.ElectionTimeoutJitter {
			t.Fatalf("reset %d: electionTimeout = %v, want in [%v, %v)", i, got, timing.ElectionTimeoutBase,
				timing.ElectionTimeoutBase+timing.ElectionTimeoutJitter)
		}
	}
}

// TestRequestVoteDisruptionCheck_UsesConfiguredBase checks that the §6
// disruption check (raft/rpc.go's RequestVote) reads rs.timing.ElectionTimeoutBase
// rather than the old minElectionTimeout constant, so a node configured with a
// short base actually disregards RequestVote for a shorter window — and one
// configured with a long base disregards it for longer. No real election runs
// here; this only exercises the comparison the disruption check makes.
func TestRequestVoteDisruptionCheck_UsesConfiguredBase(t *testing.T) {
	timing := Timing{
		ElectionTimeoutBase:   20 * time.Millisecond,
		ElectionTimeoutJitter: 5 * time.Millisecond,
		HeartbeatInterval:     5 * time.Millisecond,
	}
	rs := newTimingTestState(t, WithTiming(timing))

	rs.mu.Lock()
	rs.currentLeader = "leader1"
	rs.lastHeartbeat = time.Now()
	rs.mu.Unlock()

	// Well within the configured base: RequestVote from a higher term must be
	// ignored (term unchanged, vote not granted).
	reply := &RequestVoteReply{}
	rs.RequestVote(&RequestVoteArgs{Term: 99, CandidateID: "candidate"}, reply)
	if reply.VoteGranted {
		t.Fatalf("VoteGranted = true within the configured disruption window, want false")
	}
	if term, _ := rs.GetState(); term != 0 {
		t.Fatalf("CurrentTerm = %d, want 0 (disruption check must not adopt the higher term)", term)
	}

	// Past the configured base: the same request should now be considered.
	rs.mu.Lock()
	rs.lastHeartbeat = time.Now().Add(-2 * timing.ElectionTimeoutBase)
	rs.mu.Unlock()

	reply2 := &RequestVoteReply{}
	rs.RequestVote(&RequestVoteArgs{Term: 99, CandidateID: "candidate"}, reply2)
	if !reply2.VoteGranted {
		t.Fatalf("VoteGranted = false past the configured disruption window, want true")
	}
}
