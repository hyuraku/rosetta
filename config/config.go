package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	defaultElectionTimeout  = 150 * time.Millisecond
	defaultHeartbeatTimeout = 50 * time.Millisecond
	defaultMaxRaftState     = 1000
	defaultSnapshotInterval = 100
	defaultHTTPReadTimeout  = 10 * time.Second
	defaultHTTPWriteTimeout = 10 * time.Second

	// configFilePerm restricts the persisted config file to owner read/write only.
	configFilePerm os.FileMode = 0o600
)

type Config struct {
	NodeID     string            `json:"node_id"`
	ListenAddr string            `json:"listen_addr"`
	Peers      map[string]string `json:"peers"`
	DataDir    string            `json:"data_dir"`
	// LogLevel is reserved for future use: nothing in this codebase reads it
	// yet (KNOWN_ISSUES.md R16). The field and its JSON key are kept so a
	// config file that sets it does not fail to parse or lose the value on a
	// round trip through SaveConfig.
	LogLevel string `json:"log_level"`

	// ElectionTimeout is the election timeout base: main.go wires it to
	// raft.Timing.ElectionTimeoutBase, and the jitter added on top is the same
	// length again, so the effective range is [ElectionTimeout, 2*ElectionTimeout)
	// — 150-300ms at the defaults, matching raft's pre-R16 hardcoded constants.
	// See Validate for the 2*HeartbeatTimeout lower bound this must clear
	// (paper §5.2, broadcastTime << electionTimeout).
	ElectionTimeout time.Duration `json:"election_timeout"`
	// HeartbeatTimeout is the leader heartbeat interval: main.go wires it to
	// raft.Timing.HeartbeatInterval, which is also the node event loop's tick
	// period (KNOWN_ISSUES.md R16).
	HeartbeatTimeout time.Duration `json:"heartbeat_timeout"`

	MaxRaftState int `json:"max_raft_state"`
	// SnapshotInterval is reserved for future use: automatic snapshotting is
	// currently triggered only by MaxRaftState (see docs/log-compaction.md).
	// The field and its JSON key are kept so a config file that sets it does
	// not fail to parse or lose the value on a round trip through SaveConfig
	// (KNOWN_ISSUES.md R16).
	SnapshotInterval int `json:"snapshot_interval"`

	HTTPServerAddr   string        `json:"http_server_addr"`
	HTTPReadTimeout  time.Duration `json:"http_read_timeout"`
	HTTPWriteTimeout time.Duration `json:"http_write_timeout"`
}

func DefaultConfig() *Config {
	return &Config{
		NodeID:     "node1",
		ListenAddr: "localhost:8080",
		Peers:      make(map[string]string),
		DataDir:    "./data",
		LogLevel:   "INFO",

		ElectionTimeout:  defaultElectionTimeout,
		HeartbeatTimeout: defaultHeartbeatTimeout,

		MaxRaftState:     defaultMaxRaftState,
		SnapshotInterval: defaultSnapshotInterval,

		HTTPServerAddr:   "localhost:9080",
		HTTPReadTimeout:  defaultHTTPReadTimeout,
		HTTPWriteTimeout: defaultHTTPWriteTimeout,
	}
}

// LoadConfig reads a JSON config file starting from DefaultConfig() rather
// than a zero-valued Config, so a field the file omits keeps its documented
// default instead of silently becoming Go's zero value (0, "", nil) — which
// used to be able to fail Validate for reasons the file never expressed, or
// even pass Validate with a materially different (and unintended) setting
// than DefaultConfig's (KNOWN_ISSUES.md R16). json.Unmarshal only overwrites
// the fields present in data, leaving every omitted field at whatever
// DefaultConfig set it to.
func LoadConfig(filename string) (*Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to open config file: %v", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	config := DefaultConfig()
	if err := json.Unmarshal(data, config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %v", err)
	}

	return config, nil
}

func (c *Config) SaveConfig(filename string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	if err := os.WriteFile(filename, data, configFilePerm); err != nil {
		return fmt.Errorf("failed to write config file: %v", err)
	}

	return nil
}

func (c *Config) Validate() error {
	if c.NodeID == "" {
		return fmt.Errorf("node_id cannot be empty")
	}

	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr cannot be empty")
	}

	if c.HTTPServerAddr == "" {
		return fmt.Errorf("http_server_addr cannot be empty")
	}

	if c.ElectionTimeout <= 0 {
		return fmt.Errorf("election_timeout must be positive")
	}

	if c.HeartbeatTimeout <= 0 {
		return fmt.Errorf("heartbeat_timeout must be positive")
	}

	if c.ElectionTimeout <= c.HeartbeatTimeout {
		return fmt.Errorf("election_timeout must be greater than heartbeat_timeout")
	}

	// Paper §5.2: broadcastTime << electionTimeout, or the cluster cannot
	// reliably get a heartbeat out before followers start timing out and
	// calling elections. Requiring at least a 2x margin catches a config that
	// technically clears the check above (heartbeat=140ms, election=150ms) but
	// leaves no real room for a heartbeat to be delayed or lost. The defaults
	// (150ms/50ms) clear this with room to spare (KNOWN_ISSUES.md R16).
	if c.ElectionTimeout < 2*c.HeartbeatTimeout {
		return fmt.Errorf("election_timeout must be at least twice heartbeat_timeout")
	}

	if c.MaxRaftState <= 0 {
		return fmt.Errorf("max_raft_state must be positive")
	}

	// The following checks guard the fixed-peers cluster model that R12 falls
	// back on until dynamic membership (R14) exists: with -join rejected,
	// every node must be started with a consistent, correct -peers list, and a
	// malformed one would otherwise silently produce a wrong-sized quorum or a
	// node that can never reach a peer.
	if _, selfInPeers := c.Peers[c.NodeID]; selfInPeers {
		return fmt.Errorf("peers must not include this node's own node_id (%s)", c.NodeID)
	}

	seenAddrs := make(map[string]string, len(c.Peers))
	for id, addr := range c.Peers {
		if addr == c.ListenAddr {
			return fmt.Errorf("peer %s has the same address as listen_addr (%s)", id, addr)
		}
		if otherID, exists := seenAddrs[addr]; exists {
			return fmt.Errorf("peers %s and %s both have address %s", otherID, id, addr)
		}
		seenAddrs[addr] = id
	}

	return nil
}

func (c *Config) GetPeerIDs() []string {
	ids := make([]string, 0, len(c.Peers)+1)
	ids = append(ids, c.NodeID)
	for id := range c.Peers {
		ids = append(ids, id)
	}
	return ids
}

func (c *Config) AddPeer(nodeID, addr string) {
	if c.Peers == nil {
		c.Peers = make(map[string]string)
	}
	c.Peers[nodeID] = addr
}

func (c *Config) RemovePeer(nodeID string) {
	delete(c.Peers, nodeID)
}

func (c *Config) GetPeerAddr(nodeID string) (string, bool) {
	addr, exists := c.Peers[nodeID]
	return addr, exists
}

func (c *Config) Clone() *Config {
	peers := make(map[string]string)
	for k, v := range c.Peers {
		peers[k] = v
	}

	return &Config{
		NodeID:           c.NodeID,
		ListenAddr:       c.ListenAddr,
		Peers:            peers,
		DataDir:          c.DataDir,
		LogLevel:         c.LogLevel,
		ElectionTimeout:  c.ElectionTimeout,
		HeartbeatTimeout: c.HeartbeatTimeout,
		MaxRaftState:     c.MaxRaftState,
		SnapshotInterval: c.SnapshotInterval,
		HTTPServerAddr:   c.HTTPServerAddr,
		HTTPReadTimeout:  c.HTTPReadTimeout,
		HTTPWriteTimeout: c.HTTPWriteTimeout,
	}
}
