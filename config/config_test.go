package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testOverrideNodeID is reused by TestLoadConfig_ExplicitFieldsOverrideDefaults
// to check the file's own node_id survives LoadConfig's default-fill.
const testOverrideNodeID = "node9"

// TestLoadConfig_DefaultFillsOmittedFields covers KNOWN_ISSUES.md R16: a
// config file that sets only a few fields must come out of LoadConfig with
// every field it omitted at DefaultConfig's value, not Go's zero value. Before
// the fix, this file (which sets only node_id) would fail Validate on
// election_timeout/heartbeat_timeout/http_server_addr being empty/zero.
func TestLoadConfig_DefaultFillsOmittedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	partial := map[string]string{"node_id": "custom-node"}
	data, err := json.Marshal(partial)
	if err != nil {
		t.Fatalf("marshal partial config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := DefaultConfig()
	if cfg.NodeID != "custom-node" {
		t.Errorf("NodeID = %q, want %q (the one field the file set)", cfg.NodeID, "custom-node")
	}
	if cfg.ListenAddr != want.ListenAddr {
		t.Errorf("ListenAddr = %q, want default %q", cfg.ListenAddr, want.ListenAddr)
	}
	if cfg.HTTPServerAddr != want.HTTPServerAddr {
		t.Errorf("HTTPServerAddr = %q, want default %q", cfg.HTTPServerAddr, want.HTTPServerAddr)
	}
	if cfg.ElectionTimeout != want.ElectionTimeout {
		t.Errorf("ElectionTimeout = %v, want default %v", cfg.ElectionTimeout, want.ElectionTimeout)
	}
	if cfg.HeartbeatTimeout != want.HeartbeatTimeout {
		t.Errorf("HeartbeatTimeout = %v, want default %v", cfg.HeartbeatTimeout, want.HeartbeatTimeout)
	}
	if cfg.MaxRaftState != want.MaxRaftState {
		t.Errorf("MaxRaftState = %d, want default %d", cfg.MaxRaftState, want.MaxRaftState)
	}
	if cfg.SnapshotInterval != want.SnapshotInterval {
		t.Errorf("SnapshotInterval = %d, want default %d", cfg.SnapshotInterval, want.SnapshotInterval)
	}
	if cfg.DataDir != want.DataDir {
		t.Errorf("DataDir = %q, want default %q", cfg.DataDir, want.DataDir)
	}
}

// TestLoadConfig_ExplicitFieldsOverrideDefaults makes sure the default-fill in
// LoadConfig does not clobber fields the file does set.
func TestLoadConfig_ExplicitFieldsOverrideDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	full := DefaultConfig()
	full.NodeID = testOverrideNodeID
	full.ElectionTimeout = 300 * time.Millisecond
	full.HeartbeatTimeout = 100 * time.Millisecond
	data, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.NodeID != testOverrideNodeID {
		t.Errorf("NodeID = %q, want %q", cfg.NodeID, testOverrideNodeID)
	}
	if cfg.ElectionTimeout != 300*time.Millisecond {
		t.Errorf("ElectionTimeout = %v, want 300ms", cfg.ElectionTimeout)
	}
	if cfg.HeartbeatTimeout != 100*time.Millisecond {
		t.Errorf("HeartbeatTimeout = %v, want 100ms", cfg.HeartbeatTimeout)
	}
}

// TestValidate_RejectsElectionTimeoutBelowTwiceHeartbeat covers the new R16
// check: election_timeout must be at least 2x heartbeat_timeout (paper §5.2,
// broadcastTime << electionTimeout). 140ms/150ms clears the pre-existing
// "election > heartbeat" check but not this one.
func TestValidate_RejectsElectionTimeoutBelowTwiceHeartbeat(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HeartbeatTimeout = 140 * time.Millisecond
	cfg.ElectionTimeout = 150 * time.Millisecond

	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate: want error for election_timeout < 2*heartbeat_timeout, got nil")
	}
}

// TestValidate_AcceptsElectionTimeoutAtTwiceHeartbeat is the boundary case:
// exactly 2x must pass.
func TestValidate_AcceptsElectionTimeoutAtTwiceHeartbeat(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HeartbeatTimeout = 75 * time.Millisecond
	cfg.ElectionTimeout = 150 * time.Millisecond

	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: unexpected error at the 2x boundary: %v", err)
	}
}

// TestValidate_DefaultConfigPasses guards against DefaultConfig and Validate
// drifting apart — a config nothing overrides must always be startable.
func TestValidate_DefaultConfigPasses(t *testing.T) {
	if err := DefaultConfig().Validate(); err != nil {
		t.Errorf("DefaultConfig().Validate(): %v", err)
	}
}

// TestLoadConfig_MissingFile still fails the way it always has: LoadConfig
// must not paper over a missing/unreadable file with defaults.
func TestLoadConfig_MissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("LoadConfig: want error for a missing file, got nil")
	}
}
