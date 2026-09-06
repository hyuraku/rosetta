package raft

// The applier is the one goroutine that ever sends on rs.applyCh. Consensus
// paths (AppendEntries, the leader's commit advance, InstallSnapshot) only
// record what has become applicable and ring a bell; the applier picks the work
// up, copies it out from under rs.mu, and does the handing over with no lock
// held.
//
// Why: the previous design sent to applyCh from inside the critical section that
// had just advanced the commit index. That was safe — a committed entry could
// never be dropped — but it made the state machine's speed the node's speed: a
// slow consumer (the kvstore apply loop writing a synchronous snapshot, say)
// held rs.mu, and with it every RPC handler and the election timer, until it
// drained (KNOWN_ISSUES.md B3, TODO.md 2.5). Nothing about the safety argument
// changes here: the sends are still blocking and still in order, they simply
// happen on a goroutine that holds no lock.
//
// "applyCh is the only bridge" (CLAUDE.md) still holds, and is now narrower:
// exactly one goroutine writes to it, for the lifetime of the RaftState.

// notifyApplierLocked tells the applier there may be new work. Callers must hold
// rs.mu, and must have already made the work visible (CommitIndex advanced, or
// pendingSnapshot set) so the applier cannot look and find nothing.
//
// The send is non-blocking into a one-slot buffer, so this never blocks a lock
// holder and any number of notifications collapse into one wake-up — which is
// all that is needed, because the applier drains everything that is applicable
// each time it wakes.
func (rs *RaftState) notifyApplierLocked() {
	select {
	case rs.applyNotify <- struct{}{}:
	default:
	}
}

// takeApplyWork claims everything that is currently applicable and returns it:
// the pending snapshot (if any) followed by the commands from LastApplied+1
// through CommitIndex.
//
// LastApplied is advanced here, when the batch is claimed, not after the sends
// complete. It therefore means "reserved by the applier", not "already in the
// state machine". Two reasons:
//
//   - it makes the claim idempotent against repeated notifications, so a second
//     wake-up cannot re-send entries that are still in flight; and
//   - InstallSnapshot's monotonicity guard (KNOWN_ISSUES.md R5) tests
//     LastApplied under this same lock. With the reservation counted, a snapshot
//     is only accepted when it is strictly newer than everything the applier is
//     about to deliver, which is what keeps the two streams ordered.
//
// The cost is that entries reserved but not yet sent are lost if the process
// dies. That was already true — LastApplied is volatile state, restored to the
// snapshot boundary on restart — so the entries are simply re-applied from the
// log after a crash.
//
// The entries are copied into ApplyMsg values under the lock. The applier must
// not read rs.persistent.Log afterwards: a concurrent truncation or
// InstallSnapshot rewrites that backing array (the same hazard as
// KNOWN_ISSUES.md E2 on the send path).
func (rs *RaftState) takeApplyWork() (snapshot *ApplyMsg, commands []ApplyMsg) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	snapshot, rs.pendingSnapshot = rs.pendingSnapshot, nil

	for rs.volatile.LastApplied < rs.volatile.CommitIndex {
		next := rs.volatile.LastApplied + 1
		pos := rs.slicePos(next)
		if pos < 0 || pos >= len(rs.persistent.Log) {
			// The next entry to apply has been compacted away or is not yet
			// present. This can only happen if LastApplied lags behind the
			// snapshot boundary; refuse to index out of range and stop.
			rs.logger.Printf("applier: index %d outside live log (lastIncluded=%d, logLen=%d), stopping",
				next, rs.persistent.LastIncludedIndex, len(rs.persistent.Log))
			break
		}
		entry := rs.persistent.Log[pos]
		commands = append(commands, ApplyMsg{
			CommandValid: true,
			Command:      entry.Command,
			CommandIndex: next,
			CommandTerm:  entry.Term,
		})
		rs.volatile.LastApplied = next
	}

	return snapshot, commands
}

// applier runs until Stop. It drains everything applicable, then sleeps on the
// notification until there is more.
//
// Ordering delivered to the state machine: within one claim the snapshot goes
// first and the commands follow in index order, and claims are processed one
// after another by this single goroutine. So every command above a snapshot's
// index arrives after that snapshot. Commands at or below it can still arrive
// after — a batch claimed before the snapshot was installed is finished first —
// and the state machine drops those through the monotonicity guard it already
// needs for replayed entries (kvstore.installSnapshotFromApplyMsg / applyLoop,
// KNOWN_ISSUES.md R3/R5).
func (rs *RaftState) applier() {
	for {
		for {
			snapshot, commands := rs.takeApplyWork()
			if snapshot == nil && len(commands) == 0 {
				break
			}
			if snapshot != nil && !rs.sendApply(snapshot) {
				return
			}
			for i := range commands {
				if !rs.sendApply(&commands[i]) {
					return
				}
			}
		}

		select {
		case <-rs.stopCh:
			return
		case <-rs.applyNotify:
		}
	}
}

// sendApply hands one message to the state machine and reports whether it got
// through. It never sends once Stop has been signaled — the stop check comes
// first and is not left to select's random choice — so no send can be in flight
// when Stop returns and the owner closes applyCh (KNOWN_ISSUES.md R19).
func (rs *RaftState) sendApply(msg *ApplyMsg) bool {
	select {
	case <-rs.stopCh:
		return false
	default:
	}

	select {
	case rs.applyCh <- *msg:
		return true
	case <-rs.stopCh:
		return false
	}
}
