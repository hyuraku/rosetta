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

// Errors returned by ProposeConfigChange.
var (
	// ErrConfigChangeInProgress is returned when a configuration change is
	// already under way. The paper (§6) allows only one at a time: two
	// overlapping changes can produce two disjoint majorities and therefore two
	// leaders in the same term. "Under way" covers both a joint configuration
	// that has not reached C_new yet and a configuration entry that has been
	// appended but not yet committed.
	ErrConfigChangeInProgress = errors.New("a cluster configuration change is already in progress")
	// ErrNodeAlreadyVoter is returned when adding a node that is already a voter
	// with the same address.
	ErrNodeAlreadyVoter = errors.New("node is already a voter in the current configuration")
	// ErrNodeNotVoter is returned when removing a node that is not in the current
	// configuration.
	ErrNodeNotVoter = errors.New("node is not a voter in the current configuration")
	// ErrLastVoter is returned when removing the last voter, which would leave a
	// configuration no quorum can ever be formed in.
	ErrLastVoter = errors.New("refusing to remove the last voter from the configuration")
)

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

// Contains reports whether nodeID takes part in this configuration at all
// (either group).
func (c *ClusterConfig) Contains(nodeID string) bool {
	if c == nil {
		return false
	}
	if _, ok := c.Voters[nodeID]; ok {
		return true
	}
	_, ok := c.OldVoters[nodeID]
	return ok
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

// currentConfigLocked returns the configuration in effect and the index of the
// entry that installed it — LastIncludedIndex when it came from the snapshot
// boundary, which is committed by construction. Callers must hold rs.mu.
func (rs *RaftState) currentConfigLocked() (cfg *ClusterConfig, at int) {
	for i := len(rs.persistent.Log) - 1; i >= 0; i-- {
		if rs.persistent.Log[i].Type == entryTypeConfig {
			return rs.persistent.Config, rs.persistent.LastIncludedIndex + i + 1
		}
	}
	return rs.persistent.Config, rs.persistent.LastIncludedIndex
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

// PeerAddresses returns the address of every other server in the current
// configuration. Entries whose address is unknown (a configuration seeded from
// a bare peer-ID list) are omitted, so a caller can merge the result into an
// address book without erasing what it already knows.
func (rs *RaftState) PeerAddresses() map[string]string {
	rs.mu.RLock()
	defer rs.mu.RUnlock()

	out := make(map[string]string)
	for id, addr := range rs.persistent.Config.Members() {
		if id == rs.nodeID || addr == "" {
			continue
		}
		out[id] = addr
	}
	return out
}

// SetPeerAddresses fills in the addresses of servers that are already in the
// configuration but whose address is not known yet, and persists the result.
//
// It exists for the fixed -peers startup path: the Raft layer is handed a list
// of node IDs, while the addresses live in the process configuration. Attaching
// them to the cluster configuration is what lets a *newly added* server learn
// where its peers are from the configuration entry alone. Servers that are not
// in the configuration are ignored, and an address that is already recorded is
// never overwritten — the configuration entry the cluster agreed on wins over a
// local flag.
func (rs *RaftState) SetPeerAddresses(addrs map[string]string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	changed := rs.fillAddrsLocked(rs.persistent.Config, addrs)
	// The snapshot boundary's configuration is the fallback the log reverts to,
	// so it needs the same addresses.
	changed = rs.fillAddrsLocked(rs.persistent.SnapshotConfig, addrs) || changed
	if !changed {
		return
	}
	if err := rs.persist(); err != nil {
		rs.logger.Printf("SetPeerAddresses: persist failed: %v", err)
	}
}

// fillAddrsLocked writes addrs into the groups of cfg wherever a member's
// address is still unknown, and reports whether anything changed. Callers must
// hold rs.mu.
func (rs *RaftState) fillAddrsLocked(cfg *ClusterConfig, addrs map[string]string) bool {
	if cfg == nil {
		return false
	}
	changed := false
	for _, group := range []map[string]string{cfg.Voters, cfg.OldVoters} {
		for id, addr := range addrs {
			if addr == "" || id == rs.nodeID {
				continue
			}
			if existing, ok := group[id]; ok && existing == "" {
				group[id] = addr
				changed = true
			}
		}
	}
	return changed
}

// ---------------------------------------------------------------------------
// RaftState: driving a configuration change
// ---------------------------------------------------------------------------

// appendConfigEntryLocked appends a configuration entry and puts the new
// configuration into effect in the same critical section, because §6 requires a
// server to use a configuration "as soon as it is added to its log", not when it
// commits. Callers must hold rs.mu and must have established that appending is
// legal (leadership, for the leader-side callers).
//
// Both halves are rolled back together when the write fails, so the invariant
// "persistent.Config is the last configuration entry in the durable log" holds
// on every return path (the same discipline as appendEntryLocked, R2/C3).
func (rs *RaftState) appendConfigEntryLocked(cfg *ClusterConfig) (int, error) {
	encoded, err := cfg.encode()
	if err != nil {
		return 0, err
	}

	prevConfig := rs.persistent.Config
	rs.persistent.Config = cfg.Clone()

	index, err := rs.appendEntryLocked(encoded, entryTypeConfig)
	if err != nil {
		rs.persistent.Config = prevConfig
		return 0, err
	}
	return index, nil
}

// ProposeConfigChange starts a cluster configuration change on the leader: it
// appends the joint configuration C_old,new, which takes effect immediately. The
// rest of the transition is driven by commit progress (see
// advanceConfigChangeLocked).
//
// Removing this node itself is allowed. Per §6 the leader keeps serving until
// C_new commits and only then steps down, which is what advanceConfigChangeLocked
// does; refusing self-removal would make a cluster unable to retire its leader.
//
// A learner / non-voting catch-up phase is deliberately not implemented
// (KNOWN_ISSUES.md R20): a server added here counts towards the quorum from the
// moment C_old,new reaches a log, so adding a server whose log is far behind
// makes the cluster slower to commit until it catches up.
func (rs *RaftState) ProposeConfigChange(add bool, nodeID, addr string) (*ClusterConfig, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.state != Leader {
		return nil, ErrNotLeader
	}
	if nodeID == "" {
		return nil, errors.New("node_id is required")
	}
	// §6 relies on the leader knowing which configuration is committed before it
	// starts the next change, and a leader only knows its own commit index is
	// current once it has committed an entry of its own term (§5.4.2). The
	// election no-op makes this true within a heartbeat of every election.
	if !rs.hasCurrentTermCommittedLocked() {
		return nil, ErrNoCurrentTermCommit
	}

	current, at := rs.currentConfigLocked()
	if current.IsJoint() || at > rs.volatile.CommitIndex {
		return nil, ErrConfigChangeInProgress
	}

	newVoters := cloneAddrs(current.Voters)
	if add {
		if existing, ok := newVoters[nodeID]; ok && existing == addr {
			return nil, ErrNodeAlreadyVoter
		}
		newVoters[nodeID] = addr
	} else {
		if _, ok := newVoters[nodeID]; !ok {
			return nil, ErrNodeNotVoter
		}
		if len(newVoters) == 1 {
			return nil, ErrLastVoter
		}
		delete(newVoters, nodeID)
	}

	joint := &ClusterConfig{Voters: newVoters, OldVoters: cloneAddrs(current.Voters)}
	index, err := rs.appendConfigEntryLocked(joint)
	if err != nil {
		return nil, err
	}
	rs.syncLeaderPeersLocked()
	rs.logger.Printf("Configuration change started at index %d: joint configuration old=%v new=%v",
		index, joint.OldVoters, joint.Voters)
	return joint.Clone(), nil
}

// advanceConfigChangeLocked drives the two-phase transition of §6 from commit
// progress. Callers must hold rs.mu and must be on a path that has just moved
// the commit index (updateCommitIndex).
//
//   - once C_old,new is committed, the leader appends C_new. Only from that point
//     may decisions be made by a majority of the new configuration alone, which
//     is exactly what appending C_new does (it takes effect on append);
//   - once C_new is committed, a leader that is not in it steps down. It could
//     not have stepped down earlier: until C_new commits it is the only server
//     that can get it committed.
//
// Doing this here rather than on the applier means the transition is driven
// under the same rs.mu that advanced the commit index, so the append of C_new
// cannot interleave with a demotion; nothing here touches applyCh, and the only
// lock involved is the one the caller already holds.
func (rs *RaftState) advanceConfigChangeLocked() {
	if rs.state != Leader {
		return
	}
	current, at := rs.currentConfigLocked()
	if current == nil || at > rs.volatile.CommitIndex {
		return // not committed yet; nothing to do
	}

	if current.IsJoint() {
		final := &ClusterConfig{Voters: cloneAddrs(current.Voters)}
		index, err := rs.appendConfigEntryLocked(final)
		if err != nil {
			// The joint configuration stays in effect and the next commit
			// advance retries. Joint is a safe place to sit: agreement still
			// needs both majorities.
			rs.logger.Printf("advanceConfigChange: could not append the final configuration, "+
				"staying joint and retrying: %v", err)
			return
		}
		rs.syncLeaderPeersLocked()
		rs.logger.Printf("Joint configuration committed; appended final configuration %v at index %d",
			final.Voters, index)
		return
	}

	if !current.IsVoter(rs.nodeID) {
		rs.logger.Printf("Final configuration %v committed and no longer contains this node; stepping down",
			current.Voters)
		if err := rs.becomeFollowerLocked(rs.persistent.CurrentTerm, ""); err != nil {
			rs.logger.Printf("advanceConfigChange: step down persist failed: %v", err)
		}
	}
}

// syncLeaderPeersLocked brings the per-peer replication state in line with the
// configuration currently in effect: servers the configuration added get a
// NextIndex/MatchIndex pair, servers it dropped lose theirs. Callers must hold
// rs.mu; a no-op when this node is not the leader.
//
// A newly added server starts at NextIndex = one past our log and MatchIndex 0,
// the same seed becomeLeader uses, so the ordinary conflict back-off (or an
// InstallSnapshot) finds the right place to start replicating from.
func (rs *RaftState) syncLeaderPeersLocked() {
	if rs.leader == nil {
		return
	}
	wanted := make(map[string]bool)
	for _, peer := range rs.peerIDsLocked() {
		wanted[peer] = true
		if _, ok := rs.leader.NextIndex[peer]; !ok {
			rs.leader.NextIndex[peer] = rs.lastAbsLogIndex() + 1
			rs.leader.MatchIndex[peer] = 0
		}
	}
	for peer := range rs.leader.NextIndex {
		if !wanted[peer] {
			delete(rs.leader.NextIndex, peer)
			delete(rs.leader.MatchIndex, peer)
			delete(rs.leader.inFlight, peer)
		}
	}
}
