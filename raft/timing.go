package raft

import "time"

// Timing controls the election and heartbeat timing a RaftState uses.
// Election timeouts are randomized uniformly in
// [ElectionTimeoutBase, ElectionTimeoutBase+ElectionTimeoutJitter) on every
// reset, matching the paper's "prevent split votes" guidance (§5.2).
// HeartbeatInterval is how often a leader sends AppendEntries heartbeats.
//
// A zero-valued field is filled in from DefaultTiming by normalize, so a
// caller (WithTiming, or an omitted option entirely) can leave any subset of
// the fields unset and still get sane, pre-R16 defaults for the rest
// (KNOWN_ISSUES.md R16).
type Timing struct {
	ElectionTimeoutBase   time.Duration
	ElectionTimeoutJitter time.Duration
	HeartbeatInterval     time.Duration
}

// DefaultTiming returns the timing RaftState has always used before R16 wired
// config.Config's ElectionTimeout/HeartbeatTimeout through: a 150ms base with
// up to 150ms of jitter (150-300ms election timeouts) and a 50ms heartbeat.
func DefaultTiming() Timing {
	return Timing{
		ElectionTimeoutBase:   electionTimeoutBaseMs * time.Millisecond,
		ElectionTimeoutJitter: electionTimeoutJitterMs * time.Millisecond,
		HeartbeatInterval:     heartbeatInterval,
	}
}

// normalize fills every zero-or-negative field with DefaultTiming's value.
func (t Timing) normalize() Timing {
	d := DefaultTiming()
	if t.ElectionTimeoutBase <= 0 {
		t.ElectionTimeoutBase = d.ElectionTimeoutBase
	}
	if t.ElectionTimeoutJitter <= 0 {
		t.ElectionTimeoutJitter = d.ElectionTimeoutJitter
	}
	if t.HeartbeatInterval <= 0 {
		t.HeartbeatInterval = d.HeartbeatInterval
	}
	return t
}

// Option configures optional RaftState/RaftNode construction parameters. It is
// a functional option so NewRaftStateWithPersister and
// NewRaftNodeWithPersister can grow new knobs (like Timing) without breaking
// any of their many existing callers, none of which pass one.
type Option func(*RaftState)

// WithTiming overrides the election/heartbeat timing a RaftState uses. Zero
// fields in t fall back to DefaultTiming's values (see Timing.normalize), so
// passing a partially populated Timing — or config.Config's zero value, for a
// config file predating R16 — still yields workable timeouts.
func WithTiming(t Timing) Option {
	return func(rs *RaftState) {
		rs.timing = t.normalize()
	}
}
