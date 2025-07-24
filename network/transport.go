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

type HTTPTransport struct {
	addr        string
	client      *http.Client
	peers       map[string]string
	mu          sync.RWMutex
	raftNode    *raft.RaftNode
	server      *http.Server
}

func NewHTTPTransport(addr string) *HTTPTransport {
	return &HTTPTransport{
		addr:   addr,
		client: &http.Client{Timeout: 5 * time.Second},
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

	ht.server = &http.Server{
		Addr:         ht.addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
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
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return ht.server.Shutdown(ctx)
	}
	return nil
}

func (ht *HTTPTransport) SendRequestVote(ctx context.Context, target string, args *raft.RequestVoteArgs) (*raft.RequestVoteReply, error) {
	ht.mu.RLock()
	targetAddr, exists := ht.peers[target]
	ht.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("peer %s not found", target)
	}

	jsonData, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("http://%s/raft/requestvote", targetAddr)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ht.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var reply raft.RequestVoteReply
	err = json.Unmarshal(body, &reply)
	return &reply, err
}

func (ht *HTTPTransport) SendAppendEntries(ctx context.Context, target string, args *raft.AppendEntriesArgs) (*raft.AppendEntriesReply, error) {
	ht.mu.RLock()
	targetAddr, exists := ht.peers[target]
	ht.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("peer %s not found", target)
	}

	jsonData, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("http://%s/raft/appendentries", targetAddr)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := ht.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var reply raft.AppendEntriesReply
	err = json.Unmarshal(body, &reply)
	return &reply, err
}

func (ht *HTTPTransport) handleRequestVote(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var args raft.RequestVoteArgs
	if err := json.Unmarshal(body, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var reply raft.RequestVoteReply
	if ht.raftNode != nil {
		ht.raftNode.RequestVote(&args, &reply)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reply)
}

func (ht *HTTPTransport) handleAppendEntries(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var args raft.AppendEntriesArgs
	if err := json.Unmarshal(body, &args); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var reply raft.AppendEntriesReply
	if ht.raftNode != nil {
		ht.raftNode.AppendEntries(&args, &reply)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(reply)
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