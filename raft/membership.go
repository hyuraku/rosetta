package raft

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// entryTypeConfig labels a cluster configuration entry in LogEntry.Type. Unlike
// entryTypeCommand and entryTypeNoOp, this one is *not* observability only:
// every path that decides what the cluster looks like keys off it (see
// configAtIndexLocked), and the applier uses it to tell the state machine that
// an entry is a configuration change rather than a command it should try to
// decode (ApplyMsg.ConfigChange).
const entryTypeConfig = "config"

// ClusterConfig is a Raft cluster configuration (paper §6): the set of servers
// that vote and whose acknowledgements count towards a commit, together with the
// address each one is reachable at.
//
// Voters is the configuration being moved to. OldVoters is non-nil only while a
// change is in flight: that is the *joint* configuration C_old,new, in which
// agreement requires separate majorities of the old and the new set. A
// configuration with OldVoters == nil is an ordinary C_old or C_new.
//
// The address is carried here, not merely in the process's -peers flag, because
// a node that joins a running cluster learns of its peers only through the
// configuration entry that admits it. The address is advisory to consensus —
// quorum arithmetic only ever looks at the keys — and may be the empty string
// for a configuration seeded from a peer-ID list.
//
// Treat a *ClusterConfig handed out by this package as immutable; Clone before
// modifying.
type ClusterConfig struct {
	Voters    map[string]string `json:"voters"`
	OldVoters map[string]string `json:"old_voters,omitempty"`
}

// NewClusterConfig builds a single (non-joint) configuration from node IDs, with
// no addresses attached. This is the initial configuration of a node started
// from a fixed peer list.
func NewClusterConfig(nodeIDs []string) *ClusterConfig {
	voters := make(map[string]string, len(nodeIDs))
	for _, id := range nodeIDs {
		voters[id] = ""
	}
	return &ClusterConfig{Voters: voters}
}

// Clone returns a deep copy. A nil receiver clones to nil so callers can pass
// an absent configuration around without special cases.
func (c *ClusterConfig) Clone() *ClusterConfig {
	if c == nil {
		return nil
	}
	return &ClusterConfig{
		Voters:    cloneAddrs(c.Voters),
		OldVoters: cloneAddrs(c.OldVoters),
	}
}

func cloneAddrs(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// IsJoint reports whether this is the joint configuration C_old,new, i.e. a
// configuration change is in flight.
func (c *ClusterConfig) IsJoint() bool {
	return c != nil && c.OldVoters != nil
}

// Members returns every server that takes part in this configuration: the union
// of the new and (while joint) the old voter sets, with the new set's address
// winning when both name the same server. The union is what has to be
// *contacted* — vote requests and replication go to all of them — whereas
// agreement is evaluated per group.
func (c *ClusterConfig) Members() map[string]string {
	if c == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(c.Voters)+len(c.OldVoters))
	for id, addr := range c.OldVoters {
		out[id] = addr
	}
	for id, addr := range c.Voters {
		if addr != "" || out[id] == "" {
			out[id] = addr
		}
	}
	return out
}

// MemberIDs returns Members' keys in a stable order.
func (c *ClusterConfig) MemberIDs() []string {
	members := c.Members()
	ids := make([]string, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// IsVoter reports whether nodeID votes in the *new* configuration. It is what
// decides whether a server may campaign, and whether a leader must step down
// once C_new commits (§6).
func (c *ClusterConfig) IsVoter(nodeID string) bool {
	if c == nil {
		return false
	}
	_, ok := c.Voters[nodeID]
	return ok
}

// QuorumReached reports whether the set of servers in agree constitutes
// agreement under this configuration.
//
// This is the whole of joint consensus' safety argument in one function
// (§6): while the configuration is joint, a decision needs a majority of C_old
// *and* a majority of C_new, so no pair of decisions can be made by two
// disjoint majorities during the transition. Outside the transition it is the
// ordinary majority of the single voter set.
//
// Servers named in agree that are not in a group simply do not count towards
// that group — which is how a leader that has been voted out of C_new keeps
// replicating (it still has to get C_new committed) without counting itself.
func (c *ClusterConfig) QuorumReached(agree map[string]bool) bool {
	if c == nil {
		return false
	}
	if !majorityOf(c.Voters, agree) {
		return false
	}
	if c.OldVoters != nil && !majorityOf(c.OldVoters, agree) {
		return false
	}
	return true
}

func majorityOf(group map[string]string, agree map[string]bool) bool {
	if len(group) == 0 {
		return false
	}
	count := 0
	for id := range group {
		if agree[id] {
			count++
		}
	}
	return count*quorumDivisor > len(group)
}

// encode renders the configuration as the JSON string carried in
// LogEntry.Command. A string is used, like every other command in this
// repository, so that the entry survives the JSON round trip the HTTP transport
// performs on LogEntry.Command (an interface{}) without changing its Go type.
func (c *ClusterConfig) encode() (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("marshal cluster configuration: %w", err)
	}
	return string(data), nil
}

// decodeClusterConfig reads back what encode wrote. The command arrives as a
// string on the local path and after a JSON round trip; []byte is accepted too
// for callers that construct entries by hand.
func decodeClusterConfig(command interface{}) (*ClusterConfig, error) {
	var raw []byte
	switch v := command.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return nil, fmt.Errorf("configuration entry carries %T, want a JSON string", command)
	}

	var cfg ClusterConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("unmarshal cluster configuration: %w", err)
	}
	if cfg.Voters == nil {
		return nil, errors.New("configuration entry has no voters")
	}
	return &cfg, nil
}

// ---------------------------------------------------------------------------
// RaftState: deriving the configuration from the log
// ---------------------------------------------------------------------------

// configAtIndexLocked returns the configuration in effect at absolute log index
// idx. Callers must hold rs.mu.
//
// Paper §6: "a server always uses the latest configuration in its log,
// regardless of whether the entry is committed". So the answer is the last
// configuration entry at or below idx; if the log holds none, it is the
// configuration the snapshot boundary was taken under (SnapshotConfig).
//
// This is also what makes truncation correct without any extra bookkeeping: an
// AppendEntries conflict or a TruncateLogAfter that removes a configuration
// entry simply makes the previous one the last one, and re-running this
// function reverts to it (§6 requires exactly that).
func (rs *RaftState) configAtIndexLocked(idx int) *ClusterConfig {
	for i := len(rs.persistent.Log) - 1; i >= 0; i-- {
		absIndex := rs.persistent.LastIncludedIndex + i + 1
		if absIndex > idx {
			continue
		}
		if rs.persistent.Log[i].Type != entryTypeConfig {
			continue
		}
		decoded, err := decodeClusterConfig(rs.persistent.Log[i].Command)
		if err != nil {
			// A configuration entry we cannot read is worse than useless: acting
			// on the previous one keeps the node consistent with the servers that
			// could read it only by luck. Log it loudly and keep looking back —
			// the alternative (panicking mid-RPC) takes the node down.
			rs.logger.Printf("configAtIndex: ignoring unreadable configuration entry at index %d: %v",
				absIndex, err)
			continue
		}
		return decoded
	}
	return rs.persistent.SnapshotConfig.Clone()
}

// recomputeConfigLocked re-derives persistent.Config from the log after the log
// changed (an append, a merge that truncated a conflicting suffix, a
// truncation, or a snapshot install). Callers must hold rs.mu, and must persist
// afterwards — the configuration is part of the persistent state and has to
// reach disk in the same write as the log it was derived from.
func (rs *RaftState) recomputeConfigLocked() {
	rs.persistent.Config = rs.configAtIndexLocked(rs.lastAbsLogIndex())
}

// peerIDsLocked returns every server in the current configuration except this
// one, in a stable order. This replaces the old fixed rs.peers list everywhere a
// decision is made about who to talk to. Callers must hold rs.mu.
func (rs *RaftState) peerIDsLocked() []string {
	ids := rs.persistent.Config.MemberIDs()
	peers := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != rs.nodeID {
			peers = append(peers, id)
		}
	}
	return peers
}

// quorumReachedLocked evaluates agreement under the configuration currently in
// effect. Callers must hold rs.mu (read or write).
func (rs *RaftState) quorumReachedLocked(agree map[string]bool) bool {
	return rs.persistent.Config.QuorumReached(agree)
}

// isVoterLocked reports whether this node votes in the current configuration.
// Callers must hold rs.mu.
func (rs *RaftState) isVoterLocked() bool {
	return rs.persistent.Config.IsVoter(rs.nodeID)
}

// IsVoter reports whether this node votes in the configuration currently in
// effect.
func (rs *RaftState) IsVoter() bool {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.isVoterLocked()
}

// GetClusterConfig returns a copy of the configuration currently in effect.
func (rs *RaftState) GetClusterConfig() *ClusterConfig {
	rs.mu.RLock()
	defer rs.mu.RUnlock()
	return rs.persistent.Config.Clone()
}
