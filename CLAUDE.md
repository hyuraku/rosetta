# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Distributed key-value store implementing the Raft consensus algorithm in Go — a learning project.

**Never describe this project as production-ready in code or docs.** The happy path (leader election, log replication, HTTP KV API, crash recovery) works, but confirmed violations of Raft's safety properties remain. `KNOWN_ISSUES.md` is the live, authoritative status — read it before relying on any behavior.

## Gotchas

Things that will waste your time if you don't know them:

- **`make bench` and `go test -bench` find nothing.** There are no Go `Benchmark*` functions in this repo. Benchmarking is an external HTTP-level tool: `cd examples/benchmark && go build benchmark.go && ./benchmark -url=http://localhost:9080`. Its default read workload also doesn't actually hit anything — see `KNOWN_ISSUES.md` (R18).
- **Compaction's known safety and liveness defects on the receive path are fixed.** Group A is clear (A7, the InstallSnapshot receiver keeping a divergent suffix, is fixed in `019d33e`), and so are the three snapshot-path issues the 2026-09-06 re-audit found (`docs/raft-audit-2026-09-06.md`): cross-file generation consistency (R3, `0695b95`), metadata/payload sampled from different generations (R4, `f53617e`), and an older snapshot rolling back applied state (R5, `156510a`). The liveness gap (B3, `f873d9b`) is closed too — the handler still writes the payload under `rs.mu`, but the `applyCh` hand-off is queued for the applier instead of blocking the lock holder. Note `MaxRaftState=0` is not settable — `Validate` rejects it (`config/config.go:123-125`), despite older docs suggesting it as a workaround.
- **`RaftNode.Kill` is synchronous and must stay that way.** It joins every goroutine the node started, so the caller can close `applyCh` right after it (`kvs.Close()`) without racing an in-flight apply — the `send on closed channel` of `KNOWN_ISSUES.md` R19. Anything inside `raft/` that needs a goroutine must start it with `rs.spawn` (`raft/state.go`); a bare `go` statement is invisible to shutdown, and a spawn refused because Stop has begun leaves any slot the caller reserved for it to release.
- **Reads fail immediately after an election, on purpose.** A newly elected leader appends a current-term no-op; until that commits, reads return `ErrNoCurrentTermCommit`. This is correct ReadIndex behavior, not a bug to fix.
- **Writes only go to the leader.** PUT/DELETE against a follower return HTTP 503 with redirect information. Tests that hit an arbitrary node will flake.
- **Every path that ends up a follower must go through `becomeFollowerLocked` (`raft/state.go`)**, which adopts the term, clears the leader state, re-arms the election timer and persists. Rolling your own transition is how R6 happened: a demoted leader kept the stopped timer and never campaigned again. Inside `raft/`, reset the timer with `resetElectionTimerLocked` (requires `rs.mu`); the exported `ResetElectionTimer` takes the lock itself, so calling it under `rs.mu` deadlocks.
- **Use `raft.NewMockTransport()` in tests**, not the HTTP transport — deterministic in-memory RPC, no network timing.

## Architectural Invariants

Package layout is visible from `ls`; these design decisions are not.

- **`applyCh` is the only bridge** between the Raft layer and the KV state machine. Commands are applied in order after majority commit — nothing bypasses it. Since B3 (`f873d9b`) there is also exactly one writer: the applier goroutine (`raft/applier.go`). Consensus paths only advance `CommitIndex` (or queue a snapshot) and call `notifyApplierLocked`; the applier claims the work under `rs.mu`, copies it out, and sends with no lock held. `LastApplied` therefore means "claimed by the applier", not "in the state machine" — advancing it under the lock is what makes `InstallSnapshot`'s monotonicity guard order the snapshot ahead of every command above its index.
- **Reads use the ReadIndex protocol** (Raft dissertation §6.4), not leases: the leader captures its commit index, confirms leadership with a fresh heartbeat quorum, waits until the state machine has applied through that index, then serves from local state without appending to the log. This replaced lease-based reads and closed the D1–D3 linearizability gaps.
- **Persistence is atomic write-then-rename**, to `<data-dir>/<node-id>/raft_state.json` and `.../snapshot.json`, but only per file. The `Storage` interface (`persistence/interface.go`) exists so tests can inject storage; `FileStorage` is its on-disk implementation.
- **There is still no cross-file atomicity, so ordering carries the guarantee instead** (R3, `0695b95`). The InstallSnapshot receiver makes the KV payload durable, then the Raft boundary, then updates memory — in that order, so a crash can only leave the snapshot *ahead* of the Raft state. Startup enforces the rule: `persistence.VerifySnapshotConsistency` refuses to boot when the Raft boundary is ahead of the snapshot, and the KV apply loop ignores replayed entries at or below its `lastAppliedIndex` to absorb the other direction. Preserve that ordering in any change to the receive path. The applier split (B3) kept it: the handler still makes the payload durable and then the boundary, under one `rs.mu` acquisition, before the snapshot is queued for the applier — only the third step became asynchronous.
- **Pending KV operations carry unique IDs** (`opID`, `kvstore/store.go:583`) so client requests can be matched against applied commands across timeouts and leadership changes. Since R9 (`29bf047`) the ID is registered in `pendingOps` *before* `raft.Start` is called, not after: a commit that reaches `applyLoop` racing ahead of the registration would otherwise find nothing to deliver its result to. If `Start` reports the node is no longer leader, or the append could not be made durable, the registration is removed immediately so a failed `Start` cannot leak a `pendingOps` entry.

## Documentation Discipline

The docs previously drifted badly out of sync with the code (three contradictory "generations" coexisted until the 2026-07 overhaul). To prevent a recurrence:

- **Same-PR rule**: any PR that changes behavior in `raft/`, `kvstore/`, `network/`, `persistence/`, or `main.go` MUST update the affected docs and `KNOWN_ISSUES.md` in the same PR (including marking fixed issues as fixed, with the commit hash)
- **Verification headers**: each file under `docs/` carries a `Last verified: <date> against commit <hash>` line below its title. Update it when you verify or update a doc against the code
- **No aspirational docs**: document what the code does now. Planned or partially wired features must be labeled as such, with a `KNOWN_ISSUES.md` reference where one exists
- `docs/safety-review-2026-07-07.md` is a **frozen** point-in-time report — never edit it; record status changes in `KNOWN_ISSUES.md` instead
- **This file is subject to the same rule.** Do not mirror file listings, Makefile targets, or API endpoints here — they belong to `ls`, `make help`, and `docs/api.md`, and a copy will silently rot.

## Where to Look

- `make help` — build, test, lint, and run targets
- `docs/README.md` — documentation index (API reference, persistence, log compaction, Raft paper compliance, codebase walkthrough)
- `KNOWN_ISSUES.md` — live safety-issue status; `TODO.md` — feature roadmap
- `examples/simple-cluster/` — `start.sh` / `demo.sh` / `stop.sh` for a local 3-node cluster
