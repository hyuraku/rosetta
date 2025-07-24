package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
)

type Config struct {
	NodeID           string            `json:"node_id"`
	ListenAddr       string            `json:"listen_addr"`
	Peers            map[string]string `json:"peers"`
	DataDir          string            `json:"data_dir"`
	LogLevel         string            `json:"log_level"`
	
	ElectionTimeout  time.Duration     `json:"election_timeout"`
	HeartbeatTimeout time.Duration     `json:"heartbeat_timeout"`
	
	MaxRaftState     int               `json:"max_raft_state"`
	SnapshotInterval int               `json:"snapshot_interval"`
	
	HTTPServerAddr   string            `json:"http_server_addr"`
	HTTPReadTimeout  time.Duration     `json:"http_read_timeout"`
	HTTPWriteTimeout time.Duration     `json:"http_write_timeout"`
}

func DefaultConfig() *Config {
	return &Config{
		NodeID:           "node1",
		ListenAddr:       "localhost:8080",
		Peers:            make(map[string]string),
		DataDir:          "./data",
		LogLevel:         "INFO",
		
		ElectionTimeout:  150 * time.Millisecond,
		HeartbeatTimeout: 50 * time.Millisecond,
		
		MaxRaftState:     1000,
		SnapshotInterval: 100,
		
		HTTPServerAddr:   "localhost:9080",
		HTTPReadTimeout:  10 * time.Second,
		HTTPWriteTimeout: 10 * time.Second,
	}
}

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

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %v", err)
	}

	return &config, nil
}

func (c *Config) SaveConfig(filename string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal config: %v", err)
	}

	if err := os.WriteFile(filename, data, 0644); err != nil {
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

	if c.MaxRaftState <= 0 {
		return fmt.Errorf("max_raft_state must be positive")
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