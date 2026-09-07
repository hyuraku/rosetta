package main

import (
	"context"
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
	"time"

	"rosetta/config"
	"rosetta/kvstore"
	"rosetta/network"
	"rosetta/persistence"
	"rosetta/raft"
)

const (
	// kvPath is the base path (without trailing slash) for key-value endpoints.
	kvPath = "/kv"
	// kvBatchPath is the batch endpoint clients historically sent to even though
	// no route was ever registered for it (KNOWN_ISSUES.md R11); it must be
	// rejected explicitly rather than falling through to the "/kv/" prefix
	// handler and being silently misread as a plain PUT.
	kvBatchPath = "/kv/batch"
	// Cluster membership endpoints (KNOWN_ISSUES.md R14). They are served on the
	// client API port, alongside /kv and /status, because they are operator
	// requests rather than Raft RPCs. They are unrelated to the
	// /cluster/join|leave|nodes routes in network/discovery.go, which are
	// HTTP-level bookkeeping only and are never reached from a normal start.
	clusterAddPath    = "/cluster/add"
	clusterRemovePath = "/cluster/remove"
	clusterConfigPath = "/cluster/config"
	// minPeerParts is the minimum number of colon-separated fields in a peer spec (id:addr).
	minPeerParts = 2
	// httpShutdownTimeout bounds how long a graceful shutdown waits for the
	// client API's in-flight handlers before the rest of the stack is torn down.
	httpShutdownTimeout = 5 * time.Second
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
	mux.HandleFunc(clusterAddPath, hs.handleClusterAdd)
	mux.HandleFunc(clusterRemovePath, hs.handleClusterRemove)
	mux.HandleFunc(clusterConfigPath, hs.handleClusterConfig)

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

// Shutdown stops accepting client requests and waits for the handlers that are
// already running to finish, or for ctx to expire. It must complete before the
// Raft node is killed: a GET in flight is inside ReadIndex or waiting for the
// state machine to catch up, and both of those need the node still alive.
func (hs *HTTPServer) Shutdown(ctx context.Context) error {
	return hs.server.Shutdown(ctx)
}

func (hs *HTTPServer) handleKV(w http.ResponseWriter, r *http.Request) {
	// Batch operations are not implemented (KNOWN_ISSUES.md R11). Without this
	// check the request falls through to the "/kv/" prefix handler below and
	// handlePut silently misreads BatchArgs{Operations} as PutArgs{Key:"",
	// Value:""}, returning {"success":true} for a batch that never ran. Reject
	// loudly instead, for any method.
	if r.URL.Path == kvBatchPath {
		writeJSONError(w, http.StatusNotImplemented, "batch operations are not implemented")
		return
	}

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

// writeJSONError writes a JSON error body of the shape {"success":false,
// "error":"<msg>"}, for endpoints (like the batch rejection above) that need a
// structured body rather than the plain-text bodies http.Error produces
// elsewhere in this file (documented in docs/api.md's Response Formats
// section).
func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"error":   message,
	})
}

func (hs *HTTPServer) handlePut(w http.ResponseWriter, r *http.Request) {
	var req kvstore.PutArgs
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// An empty key is the direct symptom of the R11 batch misrouting (a
	// BatchArgs body decodes to PutArgs{Key:"",Value:""}), but it is rejected
	// unconditionally here: an empty key was never a meaningful PUT on its own.
	if req.Key == "" {
		http.Error(w, "Key required", http.StatusBadRequest)
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

// clusterChangeRequest is the body of POST /cluster/add and /cluster/remove.
// Addr is required by add (it is what the rest of the cluster will use to reach
// the new server) and ignored by remove.
type clusterChangeRequest struct {
	NodeID string `json:"node_id"`
	Addr   string `json:"addr,omitempty"`
}

func (hs *HTTPServer) handleClusterAdd(w http.ResponseWriter, r *http.Request) {
	hs.handleClusterChange(w, r, true)
}

func (hs *HTTPServer) handleClusterRemove(w http.ResponseWriter, r *http.Request) {
	hs.handleClusterChange(w, r, false)
}

// handleClusterChange proposes one membership change. Only the leader can start
// one, so a follower answers with the same 503 + X-Raft-Leader redirect the
// write path uses. Success means the configuration in the response has been
// appended and is in effect here — not that the change is complete:
//
//   - an add returns a configuration listing the server under "learners". It is
//     being caught up and is not counted by any quorum yet; the leader promotes
//     it to a voter by itself once it has caught up (KNOWN_ISSUES.md R20). The
//     caller polls GET /cluster/config until the node appears in "voters" and
//     "learners" is gone;
//   - removing a voter returns the joint configuration; the caller polls until
//     "joint" is false.
func (hs *HTTPServer) handleClusterChange(w http.ResponseWriter, r *http.Request, add bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req clusterChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.NodeID == "" {
		writeJSONError(w, http.StatusBadRequest, "node_id required")
		return
	}
	if add && req.Addr == "" {
		writeJSONError(w, http.StatusBadRequest, "addr required when adding a node")
		return
	}

	clusterConfig, err := hs.raftNode.ProposeConfigChange(add, req.NodeID, req.Addr)
	if err != nil {
		hs.writeConfigChangeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"config":  clusterConfigBody(clusterConfig),
	})
}

// writeConfigChangeError maps a ProposeConfigChange failure onto a status code:
// a redirect when this node is not (or is no longer able to act as) the leader,
// 409 when the cluster is already changing, 400 for a request that does not make
// sense against the current configuration.
func (hs *HTTPServer) writeConfigChangeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, raft.ErrNotLeader):
		leader := hs.raftNode.GetLeader()
		w.Header().Set("X-Raft-Leader", leader)
		writeJSONError(w, http.StatusServiceUnavailable,
			fmt.Sprintf("Not leader. Current leader: %s", leader))
	case errors.Is(err, raft.ErrNoCurrentTermCommit), errors.Is(err, raft.ErrConfigChangeInProgress):
		writeJSONError(w, http.StatusConflict, err.Error())
	case errors.Is(err, raft.ErrNodeAlreadyVoter),
		errors.Is(err, raft.ErrNodeNotVoter),
		errors.Is(err, raft.ErrLastVoter):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	}
}

// handleClusterConfig reports the configuration this node currently holds. It is
// answered by any node, leader or not: a configuration takes effect as soon as
// its entry reaches a log, so what a follower reports is meaningful — it is what
// that follower is using.
func (hs *HTTPServer) handleClusterConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"config":  clusterConfigBody(hs.raftNode.GetClusterConfig()),
	})
}

func clusterConfigBody(clusterConfig *raft.ClusterConfig) map[string]interface{} {
	body := map[string]interface{}{
		"joint":  clusterConfig.IsJoint(),
		"voters": map[string]string{},
	}
	if clusterConfig == nil {
		return body
	}
	body["voters"] = clusterConfig.Voters
	if clusterConfig.OldVoters != nil {
		body["old_voters"] = clusterConfig.OldVoters
	}
	// Reported only while a catch-up is under way, so an operator polling this
	// endpoint sees exactly one of "learners" (still catching up) or the node
	// among "voters" (promoted) — KNOWN_ISSUES.md R20.
	if len(clusterConfig.Learners) > 0 {
		body["learners"] = clusterConfig.Learners
	}
	return body
}

// validateJoinFlag rejects a non-empty -join value. Membership changes exist now
// (KNOWN_ISSUES.md R14), but they are made through the leader's
// POST /cluster/add, not by a joining node announcing itself: the joining node
// has no way to know whether the cluster agreed, and ClusterManager's node
// bookkeeping (network/discovery.go) is still HTTP-level only and never
// reflected in the Raft quorum. So -join stays rejected (KNOWN_ISSUES.md R12).
// The flag is kept -- reserved, not removed -- so that a caller who still passes
// it gets this explicit rejection instead of "flag provided but not defined".
// See docs/api.md for the supported way to add a node.
func validateJoinFlag(join string) error {
	if join == "" {
		return nil
	}
	return fmt.Errorf("-join is not supported (KNOWN_ISSUES.md R12); start the new node with the " +
		"existing cluster's -peers list and then POST /cluster/add to the leader")
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

// shutdown tears the node down from the outside in. Each step is only safe once
// the one above it has stopped producing work for it (KNOWN_ISSUES.md R19):
//
//  1. the client API, so no new reads/writes enter and the in-flight ones finish
//     while Raft and the state machine are still up — a GET in flight is inside
//     ReadIndex or waiting for the state machine to catch up;
//  2. inbound Raft RPCs — http.Server.Shutdown waits for the handlers already
//     inside AppendEntries/InstallSnapshot, so none is left mid-handler;
//  3. the Raft node, whose Kill joins its own goroutines, including the applier
//     that is the only sender on applyCh;
//  4. the state machine, which closes applyCh. Closing it any earlier is the
//     "send on closed channel" panic this order exists to prevent.
func shutdown(
	httpServer *HTTPServer,
	transport *network.HTTPTransport,
	raftNode *raft.RaftNode,
	kvs *kvstore.KVStore,
	clusterManager *network.ClusterManager,
) {
	clusterManager.LeaveCluster()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), httpShutdownTimeout)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP API shutdown: %v", err)
	}

	if err := transport.Stop(); err != nil {
		log.Printf("Raft transport shutdown: %v", err)
	}

	raftNode.Kill()
	kvs.Close()
}

// verifyDurableState compares the two files this node recovers from before any
// of them is opened for real.
//
// The Raft state file and the KV snapshot file are written in separate steps, so
// a crash can leave them at different generations (KNOWN_ISSUES.md R3). Startup
// is refused when the Raft boundary is ahead of the snapshot: Raft has discarded
// the log below its boundary while the state machine stops short of it, so the
// missing entries can never be delivered again and the node would silently serve
// incomplete data. This mirrors C4's fail-closed handling of an unreadable state
// file. The opposite direction is recoverable and only logged — the KV store
// ignores replayed entries it has already applied.
func verifyDurableState(storage persistence.Storage) {
	consistency, err := persistence.VerifySnapshotConsistency(storage)
	if err != nil {
		log.Fatalf("Refusing to start: %v", err)
	}
	if consistency.CompactionPending {
		log.Printf("Snapshot (index %d) is ahead of the Raft state (index %d): a log compaction did not "+
			"complete before the last shutdown. Replayed entries at or below index %d will be ignored "+
			"by the state machine.",
			consistency.SnapshotLastIncludedIndex, consistency.RaftLastIncludedIndex,
			consistency.SnapshotLastIncludedIndex)
	}
}

func main() {
	var (
		configFile = flag.String("config", "", "Configuration file path")
		nodeID     = flag.String("id", "node1", "Node ID")
		listenAddr = flag.String("listen", "localhost:8080", "Listen address for Raft")
		httpAddr   = flag.String("http", "localhost:9080", "HTTP server address")
		peers      = flag.String("peers", "", "Comma-separated list of peer addresses (format: id:addr,id:addr)")
		join       = flag.String("join", "", "Reserved and rejected (KNOWN_ISSUES.md R12): to add a node, "+
			"start it with the existing cluster's -peers list and POST /cluster/add to the leader")
	)
	flag.Parse()

	if err := validateJoinFlag(*join); err != nil {
		log.Fatalf("%v", err)
	}

	cfg := resolveConfig(*configFile, *nodeID, *listenAddr, *httpAddr, *peers)

	// Setup persistence
	dataDir := filepath.Join(cfg.DataDir, cfg.NodeID)
	storage, err := persistence.NewFileStorage(dataDir)
	if err != nil {
		log.Fatalf("Failed to create storage: %v", err)
	}
	log.Printf("Persistence enabled: data directory = %s", dataDir)

	verifyDurableState(storage)

	// Create Raft persister and KV snapshotter
	raftPersister := persistence.NewRaftPersister(storage)
	kvSnapshotter := persistence.NewKVSnapshotter(storage)
	// The leader ships InstallSnapshot bytes from the same on-disk snapshot the
	// kvstore persists, so lagging followers can parse the payload.
	raftSnapshotter := persistence.NewRaftSnapshotter(storage)

	// Create KV store with snapshotter
	kvs := kvstore.NewKVStoreWithSnapshotter(cfg.MaxRaftState, kvSnapshotter)
	applyCh := kvs.GetApplyCh()

	transport := network.NewHTTPTransport(cfg.ListenAddr)
	transport.SetPeers(cfg.Peers)

	peerIDs := cfg.GetPeerIDs()
	// cfg.ElectionTimeout is the base; the jitter added on top is the same
	// length again, giving [base, 2*base) — 150-300ms at the defaults, matching
	// raft's pre-R16 hardcoded constants. cfg.HeartbeatTimeout is the heartbeat
	// interval, which is also the node event loop's tick period
	// (raft.RaftState.HeartbeatInterval). See KNOWN_ISSUES.md R16.
	timing := raft.Timing{
		ElectionTimeoutBase:   cfg.ElectionTimeout,
		ElectionTimeoutJitter: cfg.ElectionTimeout,
		HeartbeatInterval:     cfg.HeartbeatTimeout,
	}
	raftNode, err := raft.NewRaftNodeWithPersister(cfg.NodeID, peerIDs, transport, applyCh, raftPersister, raft.WithTiming(timing))
	if err != nil {
		log.Fatalf("Failed to start Raft node: %v", err)
	}

	kvs.SetRaft(raftNode)
	// Wire the snapshotter so the leader can send InstallSnapshot to followers
	// whose required log entries have been compacted away.
	raftNode.SetSnapshotter(raftSnapshotter)
	transport.SetRaftNode(raftNode)

	// Attach the -peers addresses to the cluster configuration, then let the
	// configuration drive the transport's address book from there on. This is
	// what makes a membership change reach the network layer: a server added by
	// POST /cluster/add is carried in the configuration entry with its address,
	// so every node that replicates the entry learns how to reach it, and a
	// removed one disappears from the book on the next tick (KNOWN_ISSUES.md R14).
	raftNode.SetPeerAddresses(cfg.Peers)
	raftNode.SetPeerAddressSink(transport.SetPeers)

	if err := transport.Start(); err != nil {
		log.Fatalf("Failed to start transport: %v", err)
	}

	// validateJoinFlag above already refuses to start when -join is set, so
	// there is no HTTP join attempt here (KNOWN_ISSUES.md R12): ClusterManager
	// is used only for the fixed -peers bookkeeping and for LeaveCluster on
	// shutdown, not for joining a running cluster. It is deliberately left out of
	// the R14 membership path — the Raft cluster configuration, not this list, is
	// what decides the quorum.
	clusterManager := network.NewClusterManager(cfg.NodeID, cfg.ListenAddr)
	for id, addr := range cfg.Peers {
		clusterManager.AddNode(id, addr)
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

	shutdown(httpServer, transport, raftNode, kvs, clusterManager)

	log.Println("Shutdown complete")
}
