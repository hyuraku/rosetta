# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Distributed key-value store implementing the Raft consensus algorithm in Go — a learning project.

**Never describe this project as production-ready in code or docs.** The happy path (leader election, log replication, HTTP KV API, crash recovery) works, but confirmed violations of Raft's safety properties remain. `KNOWN_ISSUES.md` is the live, authoritative status — read it before relying on any behavior.

## Gotchas

Things that will waste your time if you don't know them:

- **`make bench` and `go test -bench` find nothing.** There are no Go `Benchmark*` functions in this repo. Benchmarking is an external HTTP-level tool: `cd examples/benchmark && go build benchmark.go && ./benchmark -url=http://localhost:9080`. Its default read workload also doesn't actually hit anything — see `KNOWN_ISSUES.md` (R18).
- **Compaction's group-A index/wiring defects are fixed, but new safety and liveness gaps remain on the same paths.** A7 (the InstallSnapshot receiver keeping a divergent suffix) is fixed in `019d33e`, so group A is clear. A 2026-09-06 re-audit (`docs/raft-audit-2026-09-06.md`) found the Raft-state and KV-snapshot files are persisted in separate, non-atomic steps (R3), that a leader can read snapshot metadata and payload from different generations (R4), and that nothing stops an older snapshot from rolling back committed KV state (R5). `KNOWN_ISSUES.md` B3 also remains on the receive path: the handler sends to `applyCh` while holding `rs.mu`, so a slow state machine freezes RPCs and the election timer. Also note `MaxRaftState=0` is not settable — `Validate` rejects it (`config/config.go:123-125`), despite older docs suggesting it as a workaround.
- **Reads fail immediately after an election, on purpose.** A newly elected leader appends a current-term no-op; until that commits, reads return `ErrNoCurrentTermCommit`. This is correct ReadIndex behavior, not a bug to fix.
- **Writes only go to the leader.** PUT/DELETE against a follower return HTTP 503 with redirect information. Tests that hit an arbitrary node will flake.
- **`RaftState.ResetElectionTimer()` must be called on every valid AppendEntries.** Missing it produces spurious elections that look like network problems.
- **Use `raft.NewMockTransport()` in tests**, not the HTTP transport — deterministic in-memory RPC, no network timing.

## Architectural Invariants

Package layout is visible from `ls`; these design decisions are not.

- **`applyCh` is the only bridge** between the Raft layer and the KV state machine. Commands are applied in order after majority commit — nothing bypasses it.
- **Reads use the ReadIndex protocol** (Raft dissertation §6.4), not leases: the leader captures its commit index, confirms leadership with a fresh heartbeat quorum, waits until the state machine has applied through that index, then serves from local state without appending to the log. This replaced lease-based reads and closed the D1–D3 linearizability gaps.
- **Persistence is atomic write-then-rename**, to `<data-dir>/<node-id>/raft_state.json` and `.../snapshot.json`, but only per file. The `Storage` interface (`persistence/interface.go`) exists so tests can inject storage; `FileStorage` is its on-disk implementation. There is no cross-file atomicity between the two files — see `KNOWN_ISSUES.md` (R3).
- **Pending KV operations carry unique IDs** (`opID`, `kvstore/store.go:583`) so client requests can be matched against applied commands across timeouts and leadership changes. The ID is registered in `pendingOps` only after `raft.Start` returns, which leaves a narrow window where a very fast apply can complete before registration — see `KNOWN_ISSUES.md` (R9).

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
