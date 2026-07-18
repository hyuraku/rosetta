package unit

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"rosetta/monitoring"
)

func TestWritePrometheusContainsGauges(t *testing.T) {
	snap := monitoring.Snapshot{
		NodeID:     nodeID1,
		State:      "Leader",
		Term:       3,
		IsLeader:   true,
		HasLeader:  true,
		LogEntries: 42,
		KVKeys:     7,
	}

	var buf bytes.Buffer
	if err := monitoring.WritePrometheus(&buf, snap); err != nil {
		t.Fatalf("WritePrometheus returned error: %v", err)
	}
	out := buf.String()

	wants := []string{
		fmt.Sprintf("rosetta_raft_term{node=%q} 3", nodeID1),
		fmt.Sprintf("rosetta_raft_is_leader{node=%q} 1", nodeID1),
		fmt.Sprintf("rosetta_raft_has_leader{node=%q} 1", nodeID1),
		fmt.Sprintf("rosetta_raft_log_entries{node=%q} 42", nodeID1),
		fmt.Sprintf("rosetta_kv_keys{node=%q} 7", nodeID1),
		fmt.Sprintf(`rosetta_raft_state{node=%q,state="Leader"} 1`, nodeID1),
		"# TYPE rosetta_raft_term gauge",
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("metrics output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestWritePrometheusEscapesLabelValues(t *testing.T) {
	snap := monitoring.Snapshot{NodeID: `n"o\de`}

	var buf bytes.Buffer
	if err := monitoring.WritePrometheus(&buf, snap); err != nil {
		t.Fatalf("WritePrometheus returned error: %v", err)
	}

	if !strings.Contains(buf.String(), `node="n\"o\\de"`) {
		t.Errorf("label value not escaped in output:\n%s", buf.String())
	}
}

func TestEvaluateLivenessAlwaysTrue(t *testing.T) {
	for _, hasLeader := range []bool{true, false} {
		report := monitoring.Evaluate(monitoring.Snapshot{HasLeader: hasLeader})
		if !report.Live {
			t.Errorf("Live should always be true (hasLeader=%v)", hasLeader)
		}
	}
}

func TestEvaluateReadinessFollowsLeader(t *testing.T) {
	ready := monitoring.Evaluate(monitoring.Snapshot{HasLeader: true, State: "Follower"})
	if !ready.Ready {
		t.Errorf("expected ready when a leader is known; reason=%q", ready.Reason)
	}
	if ready.Reason != "" {
		t.Errorf("expected empty reason when ready, got %q", ready.Reason)
	}

	notReady := monitoring.Evaluate(monitoring.Snapshot{HasLeader: false, State: "Candidate"})
	if notReady.Ready {
		t.Error("expected not-ready when no leader is known")
	}
	if notReady.Reason == "" {
		t.Error("expected a non-empty reason when not ready")
	}
}
