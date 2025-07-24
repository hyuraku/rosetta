package network

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

type NodeInfo struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type ClusterManager struct {
	mu     sync.RWMutex
	nodes  map[string]NodeInfo
	self   NodeInfo
	logger *log.Logger
}

func NewClusterManager(selfID, selfAddr string) *ClusterManager {
	cm := &ClusterManager{
		nodes:  make(map[string]NodeInfo),
		self:   NodeInfo{ID: selfID, Addr: selfAddr},
		logger: log.New(log.Writer(), "[CLUSTER] ", log.LstdFlags),
	}
	
	cm.nodes[selfID] = cm.self
	return cm
}

func (cm *ClusterManager) AddNode(id, addr string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	
	cm.nodes[id] = NodeInfo{ID: id, Addr: addr}
	cm.logger.Printf("Added node %s at %s", id, addr)
}

func (cm *ClusterManager) RemoveNode(id string) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	
	if id == cm.self.ID {
		return
	}
	
	delete(cm.nodes, id)
	cm.logger.Printf("Removed node %s", id)
}

func (cm *ClusterManager) GetNodes() map[string]NodeInfo {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	
	result := make(map[string]NodeInfo)
	for k, v := range cm.nodes {
		result[k] = v
	}
	return result
}

func (cm *ClusterManager) GetNodeAddrs() map[string]string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	
	result := make(map[string]string)
	for id, node := range cm.nodes {
		result[id] = node.Addr
	}
	return result
}

func (cm *ClusterManager) GetPeerIDs() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	
	peers := make([]string, 0, len(cm.nodes))
	for id := range cm.nodes {
		peers = append(peers, id)
	}
	return peers
}

func (cm *ClusterManager) StartDiscovery() {
	mux := http.NewServeMux()
	mux.HandleFunc("/cluster/join", cm.handleJoin)
	mux.HandleFunc("/cluster/leave", cm.handleLeave)
	mux.HandleFunc("/cluster/nodes", cm.handleNodes)
	
	server := &http.Server{
		Addr:    cm.self.Addr,
		Handler: mux,
	}
	
	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			cm.logger.Printf("Discovery server error: %v", err)
		}
	}()
}

func (cm *ClusterManager) handleJoin(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var nodeInfo NodeInfo
	if err := json.NewDecoder(r.Body).Decode(&nodeInfo); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	cm.AddNode(nodeInfo.ID, nodeInfo.Addr)
	
	nodes := cm.GetNodes()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"nodes":   nodes,
	})
}

func (cm *ClusterManager) handleLeave(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	var request struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	
	cm.RemoveNode(request.ID)
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
	})
}

func (cm *ClusterManager) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	
	nodes := cm.GetNodes()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(nodes)
}

func (cm *ClusterManager) JoinCluster(existingNodeAddr string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	
	joinData, err := json.Marshal(cm.self)
	if err != nil {
		return err
	}
	
	url := fmt.Sprintf("http://%s/cluster/join", existingNodeAddr)
	resp, err := client.Post(url, "application/json", bytes.NewBuffer(joinData))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("join failed with status %d", resp.StatusCode)
	}
	
	var response struct {
		Success bool                   `json:"success"`
		Nodes   map[string]NodeInfo    `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return err
	}
	
	cm.mu.Lock()
	for id, node := range response.Nodes {
		if id != cm.self.ID {
			cm.nodes[id] = node
		}
	}
	cm.mu.Unlock()
	
	cm.logger.Printf("Successfully joined cluster with %d nodes", len(response.Nodes))
	return nil
}

func (cm *ClusterManager) LeaveCluster() {
	nodes := cm.GetNodes()
	client := &http.Client{Timeout: 2 * time.Second}
	
	leaveData, _ := json.Marshal(map[string]string{"id": cm.self.ID})
	
	for id, node := range nodes {
		if id == cm.self.ID {
			continue
		}
		
		url := fmt.Sprintf("http://%s/cluster/leave", node.Addr)
		go func() {
			client.Post(url, "application/json", bytes.NewBuffer(leaveData))
		}()
	}
}

func (cm *ClusterManager) GetSelfInfo() NodeInfo {
	return cm.self
}

func (cm *ClusterManager) IsLeader(leaderID string) bool {
	return leaderID == cm.self.ID
}

func (cm *ClusterManager) SetLogger(logger *log.Logger) {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.logger = logger
}