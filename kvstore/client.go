package kvstore

import (
	"bytes"
	"context"
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

const (
	// clientIDBytes is the number of random bytes used for the client identifier (128-bit).
	clientIDBytes = 16
	// defaultHTTPTimeout is the timeout for HTTP requests to cluster nodes.
	defaultHTTPTimeout = 5 * time.Second
)

// Client is an HTTP client for a Rosetta cluster with leader-following retry
// and at-most-once write semantics keyed on (ClientID, SeqNum) (Raft paper
// Section 8).
//
// Writes are serialized per Client: Put and Delete hold mu from seqNum
// allocation through the end of sendRequest, so only one write from a given
// Client is ever in flight at a time. This is deliberate (KNOWN_ISSUES.md R10):
// without it, concurrent callers could allocate seqNums out of the order their
// requests actually arrive in, and the server's checkDuplicateRequest rejects a
// later-numbered request that arrives before an earlier-numbered one as a stale
// request. If you want concurrent writes, use a separate Client per concurrent
// writer -- ClientID scopes deduplication, so distinct Clients don't interfere
// with each other. Get carries no SeqNum and is not serialized.
type Client struct {
	servers []string
	leader  int
	client  *http.Client

	// Duplicate detection (Raft paper Section 8)
	clientID string     // Unique client identifier
	seqNum   int        // Monotonically increasing sequence number
	mu       sync.Mutex // Serializes Put/Delete: seqNum allocation through sendRequest
}

// ErrResultUnknown is returned, wrapped, when sendRequest exhausts every
// configured server without a definitive success or leader-redirect response
// (KNOWN_ISSUES.md R10) -- for example every server timed out or refused the
// connection. In this case the caller cannot tell whether the operation was
// applied by the cluster before the failure: see the Put and Delete doc
// comments for what that means for retrying.
var ErrResultUnknown = errors.New("result unknown: operation may or may not have been applied")

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
	b := make([]byte, clientIDBytes)
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
			Timeout: defaultHTTPTimeout,
		},
	}
}

// Put stores a key-value pair, retrying against other servers on a leader
// redirect or a network failure. It serializes with any other Put/Delete call
// on the same Client (see the Client doc comment): seqNum allocation and the
// request that carries it happen under the same lock.
//
// If every server fails without a definitive response, Put returns an error
// wrapping ErrResultUnknown: the operation may or may not have been applied.
// Calling Put again on the same Client afterward allocates a new seqNum, so the
// server's duplicate detection will not recognize it as a retry of the
// uncertain operation -- it can end up applied twice. This is safe to do only
// if the caller has some other way to tell the two apart (e.g. the value is
// idempotent). sendRequest's own internal retries -- the same request sent to a
// different server after a 503 or a network error -- are not affected: they
// reuse the same seqNum, so the server's at-most-once guarantee still holds for
// those.
func (c *Client) Put(key, value string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seqNum++
	args := PutArgs{
		Key:      key,
		Value:    value,
		ClientID: c.clientID,
		SeqNum:   c.seqNum,
	}

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

// Delete removes a key, with the same serialization, retry, and
// ErrResultUnknown contract as Put -- see its doc comment.
func (c *Client) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seqNum++
	args := DeleteArgs{
		Key:      key,
		ClientID: c.clientID,
		SeqNum:   c.seqNum,
	}

	return c.sendRequest("DELETE", "/kv/"+key, args, nil)
}

func (c *Client) sendRequest(method, path string, args, reply interface{}) error {
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

		req, err := http.NewRequestWithContext(context.Background(), method, url, body)
		if err != nil {
			c.leader = (c.leader + 1) % len(c.servers)
			continue
		}

		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		// Server addresses originate from trusted cluster configuration, not user input.
		resp, err := c.client.Do(req) //nolint:gosec // G704: request targets are trusted, configured cluster peers
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

	return fmt.Errorf("%w: no server accepted the request after %d attempt(s)", ErrResultUnknown, len(c.servers))
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

// BatchOperation, BatchArgs, and BatchReply describe the wire shape a future
// batch implementation (TODO.md "Advanced Query Features") is expected to use.
// They are kept as the reserved shape of the feature; Batch itself is rejected
// below (KNOWN_ISSUES.md R11) until a server-side /kv/batch route exists.
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

// ErrBatchNotImplemented is returned by Batch, PutBatch, and GetBatch. The
// server has no route for the "POST /kv/batch" request these methods used to
// send; it fell through to the "/kv/" prefix handler and was silently
// misinterpreted as a plain PUT with an empty key and value, returning
// {"success":true} for an operation that never ran (KNOWN_ISSUES.md R11). These
// methods now reject locally and send no HTTP request at all, rather than
// repeat that silent wrong-success behavior.
var ErrBatchNotImplemented = errors.New("batch operations are not implemented")

// Batch is not implemented; see ErrBatchNotImplemented.
func (c *Client) Batch(operations []BatchOperation) ([]string, error) {
	return nil, ErrBatchNotImplemented
}

// PutBatch is not implemented; see ErrBatchNotImplemented.
func (c *Client) PutBatch(kvPairs map[string]string) error {
	return ErrBatchNotImplemented
}

// GetBatch is not implemented; see ErrBatchNotImplemented.
func (c *Client) GetBatch(keys []string) (map[string]string, error) {
	return nil, ErrBatchNotImplemented
}
