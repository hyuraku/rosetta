// Package monitoring exposes health and Prometheus-style metrics for a node.
package monitoring

import (
	"fmt"
	"io"
)

// Snapshot is a point-in-time view of a node's observable state. The HTTP
// layer builds one of these from the Raft node and KV store, keeping this
// package free of any dependency on raft/kvstore (so it stays easy to test).
type Snapshot struct {
	NodeID     string // this node's ID
	State      string // "Follower" | "Candidate" | "Leader" | "Unknown"
	Term       int    // current Raft term
	IsLeader   bool   // whether this node currently believes it is leader
	HasLeader  bool   // whether a leader is currently known to this node
	LogEntries int    // number of entries in the Raft log
	KVKeys     int    // number of keys held in the KV store
}

// WritePrometheus renders snap as Prometheus text-format (v0.0.4) metrics.
//
// The format is deliberately hand-written to avoid pulling in the Prometheus
// client library: for a small fixed set of gauges it is a few lines, and it
// keeps the module dependency-free. If histograms (e.g. request-latency
// percentiles) are added later, switch to github.com/prometheus/client_golang.
func WritePrometheus(w io.Writer, snap Snapshot) error {
	node := escapeLabelValue(snap.NodeID)

	metrics := []struct {
		name  string
		help  string
		value int
	}{
		{"rosetta_raft_term", "Current Raft term of the node.", snap.Term},
		{"rosetta_raft_is_leader", "1 if this node is the current leader, else 0.", boolToInt(snap.IsLeader)},
		{"rosetta_raft_has_leader", "1 if a leader is currently known to this node, else 0.", boolToInt(snap.HasLeader)},
		{"rosetta_raft_log_entries", "Number of entries in the Raft log.", snap.LogEntries},
		{"rosetta_kv_keys", "Number of keys stored in the KV store.", snap.KVKeys},
	}

	for _, m := range metrics {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n", m.name, m.help); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "# TYPE %s gauge\n", m.name); err != nil {
			return err
		}
		// node is already escaped, so quote it literally (not via %q, which
		// would escape it a second time).
		if _, err := fmt.Fprintf(w, "%s{node=\"%s\"} %d\n", m.name, node, m.value); err != nil {
			return err
		}
	}

	// Node state as a labeled "info" gauge: the series for the active state
	// reads 1, so a query like max by (state) (rosetta_raft_state) shows it.
	const stateMetric = "rosetta_raft_state"
	if _, err := fmt.Fprintf(w, "# HELP %s Current role of the node (state label), value is always 1.\n", stateMetric); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "# TYPE %s gauge\n", stateMetric); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "%s{node=\"%s\",state=\"%s\"} 1\n", stateMetric, node, escapeLabelValue(snap.State))
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// escapeLabelValue escapes a string for use inside a Prometheus label value,
// per the exposition format: backslash, double-quote and newline.
func escapeLabelValue(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case '\\':
			out = append(out, '\\', '\\')
		case '"':
			out = append(out, '\\', '"')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, r)
		}
	}
	return string(out)
}
