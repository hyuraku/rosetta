# Documentation Index

> Last verified: 2026-07-21 against commit `9383cfe`.

Rosetta is an educational Raft implementation. Every document listed here is
verified against the code at the commit noted in its "Last verified" header —
if a doc contradicts the code, trust the code and please fix the doc (see the
Documentation Discipline section in [CLAUDE.md](../CLAUDE.md)).

## Start Here

1. **[Project README](../README.md)** — overview, quick start, project status
2. **[KNOWN_ISSUES.md](../KNOWN_ISSUES.md)** — live status of the confirmed safety issues; read this before relying on any behavior
3. **[Simple Cluster Example](../examples/simple-cluster/)** — hands-on 3-node cluster (`start.sh` / `demo.sh` / `stop.sh`)

## Reference

- **[API Documentation](api.md)** — HTTP API: endpoints, request/response formats, error behavior
- **[Persistence](persistence.md)** — Raft state / KV snapshot persistence and crash recovery
- **[Log Compaction](log-compaction.md)** — snapshotting and compaction design, and what is actually wired up today
- **[Raft Paper Implementation Status](raft-paper-implementation-status.md)** — clause-by-clause compliance with the Raft paper

## Analysis & Learning

- **[Safety Review (2026-07-07)](safety-review-2026-07-07.md)** — frozen point-in-time safety review report; the detailed evidence behind KNOWN_ISSUES. Do not edit — status changes go to KNOWN_ISSUES.md
- **[Codebase Textbook](textbook.md)** — in-depth guided walkthrough of the implementation

## Development

- **[Contributing Guide](../CONTRIBUTING.md)** — setup, code style, testing, PR process
- **[CLAUDE.md](../CLAUDE.md)** — project guidance for AI assistants, including the documentation discipline rules
- **[Benchmark Example](../examples/benchmark/)** — benchmark tool and performance-testing procedures

## Quick Reference

```bash
# Build
go build -o rosetta main.go

# Run node
./rosetta -id=node1 -listen=localhost:8080 -http=localhost:9080 -peers=node2:localhost:8081,node3:localhost:8082

# Test
go test ./... -v
go test ./... -race

# Start example cluster
cd examples/simple-cluster && ./start.sh
```

| Flag | Description | Example |
|------|-------------|---------|
| `-id` | Node identifier | `node1` |
| `-listen` | Raft RPC address | `localhost:8080` |
| `-http` | HTTP API address | `localhost:9080` |
| `-peers` | Peer list | `node2:localhost:8081,node3:localhost:8082` |

## External Resources

- [Raft Paper](https://raft.github.io/raft.pdf) — the extended Raft paper this project is measured against
- [Raft Visualization](http://thesecretlivesofdata.com/raft/) — interactive visualization
- [Raft Website](https://raft.github.io/)
