package network

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"rosetta/raft"
)

const (
	// rpcClientTimeout bounds outbound Raft RPC calls to peers.
	rpcClientTimeout = 5 * time.Second
	// serverReadTimeout bounds reading an inbound request.
	serverReadTimeout = 10 * time.Second
	// serverWriteTimeout bounds writing a response.
	serverWriteTimeout = 10 * time.Second
	// serverShutdownTimeout bounds graceful server shutdown.
	serverShutdownTimeout = 5 * time.Second
)

type HTTPTransport struct {
	addr     string
	client   *http.Client
	peers    map[string]string
	mu       sync.RWMutex
	raftNode *raft.RaftNode
	server   *http.Server
}

func NewHTTPTransport(addr string) *HTTPTransport {
	return &HTTPTransport{
		addr:   addr,
		client: &http.Client{Timeout: rpcClientTimeout},
		peers:  make(map[string]string),
	}
}

func (ht *HTTPTransport) SetPeers(peers map[string]string) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	ht.peers = peers
}

func (ht *HTTPTransport) SetRaftNode(node *raft.RaftNode) {
	ht.raftNode = node
}

func (ht *HTTPTransport) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/raft/requestvote", ht.handleRequestVote)
	mux.HandleFunc("/raft/appendentries", ht.handleAppendEntries)
	mux.HandleFunc("/raft/installsnapshot", ht.handleInstallSnapshot)

	ht.server = &http.Server{
		Addr:         ht.addr,
		Handler:      mux,
		ReadTimeout:  serverReadTimeout,
		WriteTimeout: serverWriteTimeout,
	}

	go func() {
		if err := ht.server.ListenAndServe(); err != http.ErrServerClosed {
			fmt.Printf("HTTP server error: %v\n", err)
		}
	}()

	return nil
}

func (ht *HTTPTransport) Stop() error {
	if ht.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer cancel()
		return ht.server.Shutdown(ctx)
	}
	return nil
}

// peerAddr resolves a peer ID to its address.
func (ht *HTTPTransport) peerAddr(target string) (string, error) {
	ht.mu.RLock()
	addr, exists := ht.peers[target]
	ht.mu.RUnlock()

	if !exists {
		return "", fmt.Errorf("peer %s not found", target)
	}
	return addr, nil
}

// sendRPC marshals args, POSTs them as JSON to url, and decodes the JSON
// response into a value of type Reply. It is shared by all Raft RPCs so the
// request/response handling stays identical across call sites.
func sendRPC[Reply any](ctx context.Context, client *http.Client, url string, args any) (*Reply, error) {
	jsonData, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// The target address comes from the operator-configured peer set, not from
	// untrusted request input, so this is not an SSRF sink.
	resp, err := client.Do(req) //nolint:gosec // G704: URL host is a trusted, operator-configured cluster peer
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var reply Reply
	if err := json.Unmarshal(body, &reply); err != nil {
		return nil, err
	}
	return &reply, nil
}

func (ht *HTTPTransport) SendRequestVote(
	ctx context.Context, target string, args *raft.RequestVoteArgs,
) (*raft.RequestVoteReply, error) {
	addr, err := ht.peerAddr(target)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("http://%s/raft/requestvote", addr)
	return sendRPC[raft.RequestVoteReply](ctx, ht.client, url, args)
}

func (ht *HTTPTransport) SendAppendEntries(
	ctx context.Context, target string, args *raft.AppendEntriesArgs,
) (*raft.AppendEntriesReply, error) {
	addr, err := ht.peerAddr(target)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("http://%s/raft/appendentries", addr)
	return sendRPC[raft.AppendEntriesReply](ctx, ht.client, url, args)
}

// decodeRPCArgs validates the method and decodes a JSON request body into a
// value of type Args, writing an HTTP error and returning ok=false on failure.
func decodeRPCArgs[Args any](w http.ResponseWriter, r *http.Request) (*Args, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return nil, false
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}

	var args Args
	if err := json.Unmarshal(body, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return &args, true
}

// handleRaftRPC decodes the request, invokes the raft handler (when one is
// wired up), and writes the JSON reply. A nil invoke means the node is not yet
// attached, in which case a zero-valued reply is returned.
func handleRaftRPC[Args any, Reply any](
	w http.ResponseWriter, r *http.Request, invoke func(*Args, *Reply) error,
) {
	args, ok := decodeRPCArgs[Args](w, r)
	if !ok {
		return
	}

	var reply Reply
	if invoke != nil {
		if err := invoke(args, &reply); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func (ht *HTTPTransport) handleRequestVote(w http.ResponseWriter, r *http.Request) {
	var invoke func(*raft.RequestVoteArgs, *raft.RequestVoteReply) error
	if ht.raftNode != nil {
		invoke = ht.raftNode.RequestVote
	}
	handleRaftRPC(w, r, invoke)
}

func (ht *HTTPTransport) handleAppendEntries(w http.ResponseWriter, r *http.Request) {
	var invoke func(*raft.AppendEntriesArgs, *raft.AppendEntriesReply) error
	if ht.raftNode != nil {
		invoke = ht.raftNode.AppendEntries
	}
	handleRaftRPC(w, r, invoke)
}

func (ht *HTTPTransport) GetAddr() string {
	return ht.addr
}

func (ht *HTTPTransport) GetPeers() map[string]string {
	ht.mu.RLock()
	defer ht.mu.RUnlock()

	result := make(map[string]string)
	for k, v := range ht.peers {
		result[k] = v
	}
	return result
}

// SendInstallSnapshot sends an InstallSnapshot RPC to a peer
func (ht *HTTPTransport) SendInstallSnapshot(
	ctx context.Context, target string, args *raft.InstallSnapshotArgs,
) (*raft.InstallSnapshotReply, error) {
	addr, err := ht.peerAddr(target)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("http://%s/raft/installsnapshot", addr)
	return sendRPC[raft.InstallSnapshotReply](ctx, ht.client, url, args)
}

// handleInstallSnapshot handles incoming InstallSnapshot RPCs
func (ht *HTTPTransport) handleInstallSnapshot(w http.ResponseWriter, r *http.Request) {
	args, ok := decodeRPCArgs[raft.InstallSnapshotArgs](w, r)
	if !ok {
		return
	}

	var reply raft.InstallSnapshotReply
	ht.raftNode.GetRaftState().InstallSnapshot(args, &reply)

	replyData, err := json.Marshal(&reply)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(replyData)
}
