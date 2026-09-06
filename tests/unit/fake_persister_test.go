package unit

import (
	"sync"

	"rosetta/raft"
)

// recordingPersister is an in-memory raft.Persister for durability tests. It
// keeps the last state that was *successfully* saved (so a test can assert what
// a crash-and-restart would recover), can be told to fail an arbitrary number of
// upcoming saves, and can run a hook inside SaveRaftState so a test can drive a
// concurrent RPC while the caller still holds rs.mu.
type recordingPersister struct {
	mu       sync.Mutex
	saved    raft.PersistentState
	failures int
	hook     func()
}

// setFailures makes the next n calls to SaveRaftState fail with errPersist.
// A negative count fails every save until it is reset.
func (p *recordingPersister) setFailures(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failures = n
}

// setHook installs a function that runs on entry to SaveRaftState, before the
// save succeeds or fails. It runs while the caller holds rs.mu, which is exactly
// what a test needs in order to prove that no other rs.mu critical section can
// interleave with a persist.
func (p *recordingPersister) setHook(f func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hook = f
}

func (p *recordingPersister) SaveRaftState(state *raft.PersistentState) error {
	p.mu.Lock()
	hook := p.hook
	fail := p.failures != 0
	if p.failures > 0 {
		p.failures--
	}
	p.mu.Unlock()

	if hook != nil {
		hook()
	}
	if fail {
		return errPersist
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.saved = clonePersistentState(state)
	return nil
}

func (p *recordingPersister) LoadRaftState() (*raft.PersistentState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	loaded := clonePersistentState(&p.saved)
	return &loaded, nil
}

// savedLog returns a copy of the log as it exists on "disk".
func (p *recordingPersister) savedLog() []raft.LogEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return clonePersistentState(&p.saved).Log
}

// clonePersistentState deep-copies the parts of the state a test inspects. The
// caller of SaveRaftState hands us a pointer straight at rs.persistent, so
// keeping the argument would alias live raft state.
func clonePersistentState(state *raft.PersistentState) raft.PersistentState {
	clone := *state
	clone.Log = append(make([]raft.LogEntry, 0, len(state.Log)), state.Log...)
	if state.VotedFor != nil {
		votedFor := *state.VotedFor
		clone.VotedFor = &votedFor
	}
	// The cluster configuration is a pointer to maps the membership paths mutate
	// in place (SetPeerAddresses), so it needs the same treatment.
	clone.Config = state.Config.Clone()
	clone.SnapshotConfig = state.SnapshotConfig.Clone()
	return clone
}
