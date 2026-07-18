# Monitoring & Observability

Rosetta exposes lightweight, dependency-free monitoring endpoints on the HTTP
API server (the `-http` address, e.g. `localhost:9080`).

## Endpoints

| Endpoint    | Purpose    | Status codes |
|-------------|------------|--------------|
| `GET /metrics` | Prometheus metrics (text exposition format v0.0.4) | `200` |
| `GET /health`  | Liveness — is the process running?                 | `200` |
| `GET /ready`   | Readiness — can this node serve client traffic?    | `200` ready / `503` not ready |

### Liveness vs. readiness

These answer two different operational questions, matching the Kubernetes
`livenessProbe` / `readinessProbe` split:

- **`/health` (liveness)** returns `200` as long as the process can answer.
  A failure here means the supervisor should **restart** the node.
- **`/ready` (readiness)** reflects whether the node can usefully participate
  in the cluster right now. A `503` here means the node should be **pulled from
  the load-balancer** but **not** restarted.

The readiness policy is **cluster-aware**: a node is ready only when a leader is
currently known to it. During a leader election no leader is known, so the node
briefly reports `503` and is taken out of rotation until consensus can make
progress again — then it rejoins automatically. The policy lives in
`monitoring/health.go` (`readinessCheck`) and can be changed there.

Both endpoints return the same JSON body:

```json
{
  "live": true,
  "ready": true,
  "details": {
    "node_id": "node1",
    "state": "Leader",
    "term": "1",
    "has_leader": "true",
    "kv_keys": "0"
  }
}
```

When not ready, `reason` explains why (e.g. `"no leader is currently known to this node"`).

## Metrics

All metrics are gauges labelled with `node` (the node ID):

| Metric | Description |
|--------|-------------|
| `rosetta_raft_term`       | Current Raft term |
| `rosetta_raft_is_leader`  | `1` if this node is the current leader, else `0` |
| `rosetta_raft_has_leader` | `1` if a leader is known to this node, else `0` |
| `rosetta_raft_log_entries`| Number of entries in the Raft log |
| `rosetta_kv_keys`         | Number of keys in the KV store |
| `rosetta_raft_state`      | Node role as a `state` label (`Leader`/`Follower`/`Candidate`), value always `1` |

### Example

```bash
curl -s localhost:9080/metrics
# HELP rosetta_raft_term Current Raft term of the node.
# TYPE rosetta_raft_term gauge
rosetta_raft_term{node="node1"} 1
...
rosetta_raft_state{node="node1",state="Leader"} 1
```

### Prometheus scrape config

```yaml
scrape_configs:
  - job_name: rosetta
    static_configs:
      - targets:
          - localhost:9080
          - localhost:9081
          - localhost:9082
```

## Implementation notes

The `/metrics` endpoint hand-writes the Prometheus text format rather than
depending on the Prometheus client library, keeping the module dependency-free.
If request-latency histograms/percentiles are added later, switch to
`github.com/prometheus/client_golang`. See `monitoring/metrics.go`.
