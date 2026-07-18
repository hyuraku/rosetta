package monitoring

import "strconv"

// HealthReport is the outcome of evaluating a node's health.
//
// It separates two distinct questions that operators (and orchestrators like
// Kubernetes) care about independently:
//
//   - Live  (liveness):  is the process functioning at all? If false, the
//     supervisor should restart the node.
//   - Ready (readiness): can this node usefully participate in the cluster
//     right now? If false, it should be pulled out of the load-balancer /
//     not sent client traffic, but NOT restarted.
type HealthReport struct {
	Live    bool              `json:"live"`
	Ready   bool              `json:"ready"`
	Reason  string            `json:"reason,omitempty"`
	Details map[string]string `json:"details"`
}

// Evaluate turns an observed Snapshot into a HealthReport.
//
// Liveness is trivially true here: if this code is running to answer the
// request, the process is alive. The interesting decision — readiness — is
// delegated to readinessCheck below.
func Evaluate(snap Snapshot) HealthReport {
	ready, reason := readinessCheck(snap)

	return HealthReport{
		Live:   true,
		Ready:  ready,
		Reason: reason,
		Details: map[string]string{
			"node_id":    snap.NodeID,
			"state":      snap.State,
			"term":       strconv.Itoa(snap.Term),
			"has_leader": strconv.FormatBool(snap.HasLeader),
			"kv_keys":    strconv.Itoa(snap.KVKeys),
		},
	}
}

// readinessCheck decides whether this node is ready to serve client traffic.
//
// Policy: cluster-aware. The node is ready only when a leader is currently
// known to it (snap.HasLeader). During a leader election no leader is known,
// so the node reports not-ready and should be pulled from the load-balancer
// until consensus can make progress again; it rejoins automatically once a
// leader is established. A candidate with no known leader is therefore not
// ready. (Alternatives considered and rejected: leader-only, which would mark
// read-serving followers not-ready; and always-ready, under which a partitioned
// node with no leader would still accept traffic it cannot serve.)
func readinessCheck(snap Snapshot) (ready bool, reason string) {
	if !snap.HasLeader {
		return false, "no leader is currently known to this node"
	}
	return true, ""
}
