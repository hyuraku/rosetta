package kvstore

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type Client struct {
	servers []string
	leader  int
	client  *http.Client

	// Duplicate detection (Raft paper Section 8)
	clientID string     // Unique client identifier
	seqNum   int        // Monotonically increasing sequence number
	mu       sync.Mutex // Protects seqNum
}

type PutArgs struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	ClientID string `json:"client_id,omitempty"`
	SeqNum   int    `json:"seq_num,omitempty"`
}

type GetArgs struct {
	Key string `json:"key"`
}

type DeleteArgs struct {
	Key      string `json:"key"`
	ClientID string `json:"client_id,omitempty"`
	SeqNum   int    `json:"seq_num,omitempty"`
}

type Reply struct {
	Success bool   `json:"success"`
	Value   string `json:"value,omitempty"`
	Error   string `json:"error,omitempty"`
}

// generateClientID creates a random client identifier
func generateClientID() string {
	b := make([]byte, 16) // 128-bit identifier
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based ID if crypto/rand fails
		return fmt.Sprintf("client-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func NewClient(servers []string) *Client {
	return &Client{
		servers:  servers,
		leader:   0,
		clientID: generateClientID(),
		seqNum:   0,
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (c *Client) Put(key, value string) error {
	c.mu.Lock()
	c.seqNum++
	args := PutArgs{
		Key:      key,
		Value:    value,
		ClientID: c.clientID,
		SeqNum:   c.seqNum,
	}
	c.mu.Unlock()

	return c.sendRequest("PUT", "/kv", args, nil)
}

func (c *Client) Get(key string) (string, error) {
	args := GetArgs{Key: key}
	var reply Reply
	err := c.sendRequest("GET", "/kv/"+key, args, &reply)
	if err != nil {
		return "", err
	}
	if !reply.Success {
		return "", errors.New(reply.Error)
	}
	return reply.Value, nil
}

func (c *Client) Delete(key string) error {
	c.mu.Lock()
	c.seqNum++
	args := DeleteArgs{
		Key:      key,
		ClientID: c.clientID,
		SeqNum:   c.seqNum,
	}
	c.mu.Unlock()

	return c.sendRequest("DELETE", "/kv/"+key, args, nil)
}

func (c *Client) sendRequest(method, path string, args interface{}, reply interface{}) error {
	for i := 0; i < len(c.servers); i++ {
		server := c.servers[c.leader]
		url := fmt.Sprintf("http://%s%s", server, path)

		var body io.Reader
		if args != nil {
			jsonData, err := json.Marshal(args)
			if err != nil {
				return err
			}
			body = bytes.NewBuffer(jsonData)
		}

		req, err := http.NewRequest(method, url, body)
		if err != nil {
			c.leader = (c.leader + 1) % len(c.servers)
			continue
		}

		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.client.Do(req)
		if err != nil {
			c.leader = (c.leader + 1) % len(c.servers)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		if err != nil {
			c.leader = (c.leader + 1) % len(c.servers)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			if reply != nil {
				return json.Unmarshal(respBody, reply)
			}
			return nil
		}

		if resp.StatusCode == http.StatusServiceUnavailable {
			c.leader = (c.leader + 1) % len(c.servers)
			continue
		}

		var errorReply Reply
		if json.Unmarshal(respBody, &errorReply) == nil && errorReply.Error != "" {
			return errors.New(errorReply.Error)
		}

		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return errors.New("no available servers")
}

func (c *Client) SetServers(servers []string) {
	c.servers = servers
	c.leader = 0
}

func (c *Client) GetCurrentLeader() string {
	if c.leader >= 0 && c.leader < len(c.servers) {
		return c.servers[c.leader]
	}
	return ""
}

func (c *Client) Close() {
	if c.client != nil {
		c.client.CloseIdleConnections()
	}
}

type BatchOperation struct {
	Op    string `json:"op"`
	Key   string `json:"key"`
	Value string `json:"value,omitempty"`
}

type BatchArgs struct {
	Operations []BatchOperation `json:"operations"`
}

type BatchReply struct {
	Success bool     `json:"success"`
	Results []string `json:"results,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func (c *Client) Batch(operations []BatchOperation) ([]string, error) {
	args := BatchArgs{Operations: operations}
	var reply BatchReply
	err := c.sendRequest("POST", "/kv/batch", args, &reply)
	if err != nil {
		return nil, err
	}
	if !reply.Success {
		return nil, errors.New(reply.Error)
	}
	return reply.Results, nil
}

func (c *Client) PutBatch(kvPairs map[string]string) error {
	operations := make([]BatchOperation, 0, len(kvPairs))
	for key, value := range kvPairs {
		operations = append(operations, BatchOperation{
			Op:    "PUT",
			Key:   key,
			Value: value,
		})
	}

	_, err := c.Batch(operations)
	return err
}

func (c *Client) GetBatch(keys []string) (map[string]string, error) {
	operations := make([]BatchOperation, len(keys))
	for i, key := range keys {
		operations[i] = BatchOperation{
			Op:  "GET",
			Key: key,
		}
	}

	results, err := c.Batch(operations)
	if err != nil {
		return nil, err
	}

	resultMap := make(map[string]string)
	for i, key := range keys {
		if i < len(results) && results[i] != "" {
			resultMap[key] = results[i]
		}
	}

	return resultMap, nil
}
