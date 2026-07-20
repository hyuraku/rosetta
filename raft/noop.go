package raft

// NoOpCommand is the sentinel Command carried by the no-op log entry that a
// newly elected leader appends to its own log. Committing an entry from the
// current term serves two purposes:
//
//   - Figure 8 / §5.4.2: a leader may not consider an entry from a previous term
//     committed merely because it is stored on a majority. Committing a
//     current-term entry (this no-op) lets the commit index safely advance over
//     those inherited entries via the Log Matching Property.
//   - §6.4 (ReadIndex): a linearizable read may only start once the leader has
//     committed at least one entry in its current term, so that its commit index
//     is known to reflect all previously committed entries. The no-op guarantees
//     this precondition is reached shortly after every election.
//
// State machines must recognize this value, advance their applied index over it,
// and otherwise skip it — the no-op mutates no state.
const NoOpCommand = "__raft_noop__"

// entryTypeNoOp labels the no-op entry's LogEntry.Type for observability only.
// The state machine keys off the Command value (NoOpCommand), not this Type,
// because ApplyMsg does not carry the entry's Type field.
const entryTypeNoOp = "noop"

// becomeLeader promotes this node from Candidate to Leader. Callers must hold
// rs.mu. It appends a current-term no-op (see NoOpCommand), initializes the
// leader's per-peer replication state, and stops the election timer. The no-op
// is appended before initializeLeaderState so the peers' NextIndex accounts for
// it and normal replication carries it to followers.
func (rs *RaftState) becomeLeader() {
	rs.state = Leader
	rs.currentLeader = rs.nodeID
	rs.appendNoOpLocked()
	rs.initializeLeaderState()
	rs.electionTimer.Stop()
}

// appendNoOpLocked appends a no-op entry for the current term. Callers must hold
// rs.mu. The entry is persisted like any other log entry so it survives a crash.
func (rs *RaftState) appendNoOpLocked() {
	index := rs.lastAbsLogIndex() + 1
	rs.persistent.Log = append(rs.persistent.Log, LogEntry{
		Term:    rs.persistent.CurrentTerm,
		Index:   index,
		Command: NoOpCommand,
		Type:    entryTypeNoOp,
	})
	if err := rs.persist(); err != nil {
		rs.logger.Printf("appendNoOp: persist failed: %v", err)
	}
}
