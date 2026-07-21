package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"rosetta/config"
	"rosetta/kvstore"
	"rosetta/network"
	"rosetta/persistence"
	"rosetta/raft"
)

const (
	// kvPath is the base path (without trailing slash) for key-value endpoints.
	kvPath = "/kv"
	// minPeerParts is the minimum number of colon-separated fields in a peer spec (id:addr).
	minPeerParts = 2
)

type HTTPServer struct {
	kvStore  *kvstore.KVStore
	raftNode *raft.RaftNode
	config   *config.Config
	server   *http.Server
}

func NewHTTPServer(kvs *kvstore.KVStore, raftNode *raft.RaftNode, cfg *config.Config) *HTTPServer {
	hs := &HTTPServer{
		kvStore:  kvs,
		raftNode: raftNode,
		config:   cfg,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/kv/", hs.handleKV)
	mux.HandleFunc("/kv", hs.handleKV)
	mux.HandleFunc("/status", hs.handleStatus)
	mux.HandleFunc("/leader", hs.handleLeader)

	hs.server = &http.Server{
		Addr:         cfg.HTTPServerAddr,
		Handler:      mux,
		ReadTimeout:  cfg.HTTPReadTimeout,
		WriteTimeout: cfg.HTTPWriteTimeout,
	}

	return hs
}

func (hs *HTTPServer) Start() error {
	log.Printf("Starting HTTP server on %s", hs.config.HTTPServerAddr)
	return hs.server.ListenAndServe()
}

func (hs *HTTPServer) handleKV(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "PUT", "POST":
		hs.handlePut(w, r)
	case "GET":
		hs.handleGet(w, r)
	case "DELETE":
		hs.handleDelete(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (hs *HTTPServer) handlePut(w http.ResponseWriter, r *http.Request) {
	var req kvstore.PutArgs
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := hs.kvStore.PutWithSession(req.Key, req.Value, req.ClientID, req.SeqNum); err != nil {
		if strings.Contains(err.Error(), "not leader") {
			leader := hs.raftNode.GetLeader()
			w.Header().Set("X-Raft-Leader", leader)
			http.Error(w, fmt.Sprintf("Not leader. Current leader: %s", leader), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	})
}

func (hs *HTTPServer) handleGet(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/kv/")
	if key == "" || key == kvPath {
		http.Error(w, "Key required", http.StatusBadRequest)
		return
	}

	value, err := hs.kvStore.Get(key)
	if err != nil {
		if strings.Contains(err.Error(), "not leader") {
			leader := hs.raftNode.GetLeader()
			w.Header().Set("X-Raft-Leader", leader)
			http.Error(w, fmt.Sprintf("Not leader. Current leader: %s", leader), http.StatusServiceUnavailable)
			return
		}
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, "Key not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"value":   value,
	})
}

func (hs *HTTPServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.URL.Path, "/kv/")
	if key == "" || key == kvPath {
		http.Error(w, "Key required", http.StatusBadRequest)
		return
	}

	// Duplicate-detection fields are carried in the request body when present.
	// A missing or empty body keeps backward-compatible behavior (no dedup).
	var req kvstore.DeleteArgs
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := hs.kvStore.DeleteWithSession(key, req.ClientID, req.SeqNum); err != nil {
		if strings.Contains(err.Error(), "not leader") {
			leader := hs.raftNode.GetLeader()
			w.Header().Set("X-Raft-Leader", leader)
			http.Error(w, fmt.Sprintf("Not leader. Current leader: %s", leader), http.StatusServiceUnavailable)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	})
}

func (hs *HTTPServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	term, isLeader := hs.raftNode.GetState()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"node_id":   hs.raftNode.GetNodeID(),
		"term":      term,
		"is_leader": isLeader,
		"log_size":  hs.raftNode.GetLogLength(),
	})
}

func (hs *HTTPServer) handleLeader(w http.ResponseWriter, r *http.Request) {
	leader := hs.raftNode.GetLeader()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"leader": leader,
	})
}

func parsePeers(peers string) map[string]string {
	result := make(map[string]string)
	for _, peer := range strings.Split(peers, ",") {
		parts := strings.Split(peer, ":")
		if len(parts) >= minPeerParts {
			peerID := parts[0]
			peerAddr := strings.Join(parts[1:], ":")
			result[peerID] = peerAddr
		}
	}
	return result
}

// resolveConfig loads configuration from a file when configFile is set,
// otherwise builds it from the individual flags. It aborts the process on an
// invalid configuration.
func resolveConfig(configFile, nodeID, listenAddr, httpAddr, peers string) *config.Config {
	var cfg *config.Config

	if configFile != "" {
		var err error
		cfg, err = config.LoadConfig(configFile)
		if err != nil {
			log.Fatalf("Failed to load config: %v", err)
		}
	} else {
		cfg = config.DefaultConfig()
		cfg.NodeID = nodeID
		cfg.ListenAddr = listenAddr
		cfg.HTTPServerAddr = httpAddr

		if peers != "" {
			cfg.Peers = parsePeers(peers)
		}
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("Invalid configuration: %v", err)
	}

	return cfg
}

func main() {
	var (
		configFile = flag.String("config", "", "Configuration file path")
		nodeID     = flag.String("id", "node1", "Node ID")
		listenAddr = flag.String("listen", "localhost:8080", "Listen address for Raft")
		httpAddr   = flag.String("http", "localhost:9080", "HTTP server address")
		peers      = flag.String("peers", "", "Comma-separated list of peer addresses (format: id:addr,id:addr)")
		join       = flag.String("join", "", "Join existing cluster by connecting to this address")
	)
	flag.Parse()

	cfg := resolveConfig(*configFile, *nodeID, *listenAddr, *httpAddr, *peers)

	// Setup persistence
	dataDir := filepath.Join(cfg.DataDir, cfg.NodeID)
	storage, err := persistence.NewFileStorage(dataDir)
	if err != nil {
		log.Fatalf("Failed to create storage: %v", err)
	}
	log.Printf("Persistence enabled: data directory = %s", dataDir)

	// Create Raft persister and KV snapshotter
	raftPersister := persistence.NewRaftPersister(storage)
	kvSnapshotter := persistence.NewKVSnapshotter(storage)

	// Create KV store with snapshotter
	kvs := kvstore.NewKVStoreWithSnapshotter(cfg.MaxRaftState, kvSnapshotter)
	applyCh := kvs.GetApplyCh()

	transport := network.NewHTTPTransport(cfg.ListenAddr)
	transport.SetPeers(cfg.Peers)

	peerIDs := cfg.GetPeerIDs()
	raftNode, err := raft.NewRaftNodeWithPersister(cfg.NodeID, peerIDs, transport, applyCh, raftPersister)
	if err != nil {
		log.Fatalf("Failed to start Raft node: %v", err)
	}

	kvs.SetRaft(raftNode)
	transport.SetRaftNode(raftNode)

	if err := transport.Start(); err != nil {
		log.Fatalf("Failed to start transport: %v", err)
	}

	clusterManager := network.NewClusterManager(cfg.NodeID, cfg.ListenAddr)
	for id, addr := range cfg.Peers {
		clusterManager.AddNode(id, addr)
	}

	if *join != "" {
		if err := clusterManager.JoinCluster(*join); err != nil {
			log.Printf("Failed to join cluster: %v", err)
		}
	}

	httpServer := NewHTTPServer(kvs, raftNode, cfg)

	go func() {
		if err := httpServer.Start(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	log.Printf("Node %s started successfully", cfg.NodeID)
	log.Printf("Raft listening on %s", cfg.ListenAddr)
	log.Printf("HTTP API listening on %s", cfg.HTTPServerAddr)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	log.Println("Shutting down...")

	clusterManager.LeaveCluster()
	raftNode.Kill()
	_ = transport.Stop()
	kvs.Close()

	log.Println("Shutdown complete")
}
