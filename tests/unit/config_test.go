package unit

import (
	"testing"
	"time"

	"rosetta/config"
)

func TestDefaultConfig(t *testing.T) {
	cfg := config.DefaultConfig()

	if cfg == nil {
		t.Fatal("DefaultConfig returned nil")
	}

	if cfg.NodeID != "node1" {
		t.Errorf("Expected default NodeID to be 'node1', got %s", cfg.NodeID)
	}

	if cfg.ListenAddr != "localhost:8080" {
		t.Errorf("Expected default ListenAddr to be 'localhost:8080', got %s", cfg.ListenAddr)
	}

	if cfg.HTTPServerAddr != "localhost:9080" {
		t.Errorf("Expected default HTTPServerAddr to be 'localhost:9080', got %s", cfg.HTTPServerAddr)
	}

	if cfg.ElectionTimeout != 150*time.Millisecond {
		t.Errorf("Expected default ElectionTimeout to be 150ms, got %v", cfg.ElectionTimeout)
	}

	if cfg.HeartbeatTimeout != 50*time.Millisecond {
		t.Errorf("Expected default HeartbeatTimeout to be 50ms, got %v", cfg.HeartbeatTimeout)
	}

	if cfg.MaxRaftState != 1000 {
		t.Errorf("Expected default MaxRaftState to be 1000, got %d", cfg.MaxRaftState)
	}

	if cfg.Peers == nil {
		t.Error("Expected Peers map to be initialized")
	}
}

func TestConfigValidation(t *testing.T) {
	cfg := config.DefaultConfig()

	err := cfg.Validate()
	if err != nil {
		t.Errorf("Expected default config to be valid, got error: %v", err)
	}

	cfg.NodeID = ""
	err = cfg.Validate()
	if err == nil {
		t.Error("Expected validation to fail for empty NodeID")
	}

	cfg = config.DefaultConfig()
	cfg.ListenAddr = ""
	err = cfg.Validate()
	if err == nil {
		t.Error("Expected validation to fail for empty ListenAddr")
	}

	cfg = config.DefaultConfig()
	cfg.HTTPServerAddr = ""
	err = cfg.Validate()
	if err == nil {
		t.Error("Expected validation to fail for empty HTTPServerAddr")
	}

	cfg = config.DefaultConfig()
	cfg.ElectionTimeout = 0
	err = cfg.Validate()
	if err == nil {
		t.Error("Expected validation to fail for zero ElectionTimeout")
	}

	cfg = config.DefaultConfig()
	cfg.HeartbeatTimeout = 0
	err = cfg.Validate()
	if err == nil {
		t.Error("Expected validation to fail for zero HeartbeatTimeout")
	}

	cfg = config.DefaultConfig()
	cfg.ElectionTimeout = 50 * time.Millisecond
	cfg.HeartbeatTimeout = 100 * time.Millisecond
	err = cfg.Validate()
	if err == nil {
		t.Error("Expected validation to fail when ElectionTimeout <= HeartbeatTimeout")
	}

	cfg = config.DefaultConfig()
	cfg.MaxRaftState = 0
	err = cfg.Validate()
	if err == nil {
		t.Error("Expected validation to fail for zero MaxRaftState")
	}
}

func TestPeerManagement(t *testing.T) {
	cfg := config.DefaultConfig()

	cfg.AddPeer("node2", "localhost:8081")
	cfg.AddPeer("node3", "localhost:8082")

	if len(cfg.Peers) != 2 {
		t.Errorf("Expected 2 peers, got %d", len(cfg.Peers))
	}

	addr, exists := cfg.GetPeerAddr("node2")
	if !exists {
		t.Error("Expected node2 to exist")
	}
	if addr != "localhost:8081" {
		t.Errorf("Expected node2 address to be 'localhost:8081', got %s", addr)
	}

	cfg.RemovePeer("node2")
	if len(cfg.Peers) != 1 {
		t.Errorf("Expected 1 peer after removal, got %d", len(cfg.Peers))
	}

	_, exists = cfg.GetPeerAddr("node2")
	if exists {
		t.Error("Expected node2 to be removed")
	}
}

func TestGetPeerIDs(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NodeID = "node1"
	cfg.AddPeer("node2", "localhost:8081")
	cfg.AddPeer("node3", "localhost:8082")

	peerIDs := cfg.GetPeerIDs()

	if len(peerIDs) != 3 {
		t.Errorf("Expected 3 peer IDs (including self), got %d", len(peerIDs))
	}

	idSet := make(map[string]bool)
	for _, id := range peerIDs {
		idSet[id] = true
	}

	expectedIDs := []string{"node1", "node2", "node3"}
	for _, expectedID := range expectedIDs {
		if !idSet[expectedID] {
			t.Errorf("Expected ID %s to be in peer IDs", expectedID)
		}
	}
}

func TestConfigClone(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.NodeID = "test-node"
	cfg.AddPeer("node2", "localhost:8081")
	cfg.AddPeer("node3", "localhost:8082")

	cloned := cfg.Clone()

	if cloned == nil {
		t.Fatal("Clone returned nil")
	}

	if cloned.NodeID != cfg.NodeID {
		t.Errorf("Expected cloned NodeID to be %s, got %s", cfg.NodeID, cloned.NodeID)
	}

	if len(cloned.Peers) != len(cfg.Peers) {
		t.Errorf("Expected cloned peers length to be %d, got %d", len(cfg.Peers), len(cloned.Peers))
	}

	for id, addr := range cfg.Peers {
		if cloned.Peers[id] != addr {
			t.Errorf("Expected cloned peer %s address to be %s, got %s", id, addr, cloned.Peers[id])
		}
	}

	cloned.AddPeer("node4", "localhost:8083")

	if len(cfg.Peers) == len(cloned.Peers) {
		t.Error("Expected original config to be unaffected by changes to cloned config")
	}
}

func TestConfigPeersNilSafety(t *testing.T) {
	cfg := &config.Config{
		NodeID:           "test",
		ListenAddr:       "localhost:8080",
		HTTPServerAddr:   "localhost:9080",
		ElectionTimeout:  150 * time.Millisecond,
		HeartbeatTimeout: 50 * time.Millisecond,
		MaxRaftState:     1000,
	}

	cfg.AddPeer("node2", "localhost:8081")

	if cfg.Peers == nil {
		t.Error("Expected Peers map to be initialized after AddPeer")
	}

	if len(cfg.Peers) != 1 {
		t.Errorf("Expected 1 peer, got %d", len(cfg.Peers))
	}

	addr, exists := cfg.GetPeerAddr("node2")
	if !exists || addr != "localhost:8081" {
		t.Error("Expected to find node2 with correct address")
	}
}

func TestConfigFields(t *testing.T) {
	cfg := config.DefaultConfig()

	if cfg.DataDir != "./data" {
		t.Errorf("Expected default DataDir to be './data', got %s", cfg.DataDir)
	}

	if cfg.LogLevel != "INFO" {
		t.Errorf("Expected default LogLevel to be 'INFO', got %s", cfg.LogLevel)
	}

	if cfg.SnapshotInterval != 100 {
		t.Errorf("Expected default SnapshotInterval to be 100, got %d", cfg.SnapshotInterval)
	}

	if cfg.HTTPReadTimeout != 10*time.Second {
		t.Errorf("Expected default HTTPReadTimeout to be 10s, got %v", cfg.HTTPReadTimeout)
	}

	if cfg.HTTPWriteTimeout != 10*time.Second {
		t.Errorf("Expected default HTTPWriteTimeout to be 10s, got %v", cfg.HTTPWriteTimeout)
	}
}
