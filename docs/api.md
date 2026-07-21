# API Documentation

This document provides detailed information about the Rosetta HTTP API.

## Base URL

```
http://<node-address>:<http-port>
```

Example: `http://localhost:9080`

## Endpoints

### 1. Store Key-Value Pair

Store or update a key-value pair in the distributed store.

**Endpoint:** `PUT /kv`

**Request Body:**
```json
{
  "key": "string",
  "value": "string"
}
```

**Success Response:**
- **Code:** 200 OK
- **Content:**
```json
{
  "status": "success",
  "key": "string"
}
```

**Error Responses:**

- **Code:** 400 Bad Request
  - Invalid JSON or missing required fields
  ```json
  {
    "error": "invalid request body"
  }
  ```

- **Code:** 503 Service Unavailable
  - Node is not the leader
  ```json
  {
    "error": "not leader",
    "leader": "node2:localhost:8081"
  }
  ```

- **Code:** 500 Internal Server Error
  - Consensus timeout or other internal error
  ```json
  {
    "error": "consensus timeout"
  }
  ```

**Example:**
```bash
curl -X PUT http://localhost:9080/kv \
  -H "Content-Type: application/json" \
  -d '{"key":"user:123","value":"john_doe"}'
```

---

### 2. Retrieve Value by Key

Get the value associated with a specific key.

**Endpoint:** `GET /kv/{key}`

**URL Parameters:**
- `key` (string, required) - The key to retrieve

**Success Response:**
- **Code:** 200 OK
- **Content:**
```json
{
  "key": "string",
  "value": "string"
}
```

**Error Responses:**

- **Code:** 404 Not Found
  - Key does not exist
  ```json
  {
    "error": "key not found"
  }
  ```

- **Code:** 500 Internal Server Error
  ```json
  {
    "error": "internal server error"
  }
  ```

**Example:**
```bash
curl http://localhost:9080/kv/user:123
```

---

### 3. Delete Key

Remove a key-value pair from the store.

**Endpoint:** `DELETE /kv/{key}`

**URL Parameters:**
- `key` (string, required) - The key to delete

**Success Response:**
- **Code:** 200 OK
- **Content:**
```json
{
  "status": "deleted",
  "key": "string"
}
```

**Error Responses:**

- **Code:** 404 Not Found
  - Key does not exist
  ```json
  {
    "error": "key not found"
  }
  ```

- **Code:** 503 Service Unavailable
  - Node is not the leader
  ```json
  {
    "error": "not leader",
    "leader": "node2:localhost:8081"
  }
  ```

- **Code:** 500 Internal Server Error
  ```json
  {
    "error": "consensus timeout"
  }
  ```

**Example:**
```bash
curl -X DELETE http://localhost:9080/kv/user:123
```

---

### 4. Node Status

Get the current status of the Raft node.

**Endpoint:** `GET /status`

**Success Response:**
- **Code:** 200 OK
- **Content:**
```json
{
  "node_id": "string",
  "state": "string",
  "term": 0,
  "log_length": 0,
  "commit_index": 0,
  "is_leader": false,
  "leader_id": "string"
}
```

**Field Descriptions:**
- `node_id`: Unique identifier for this node
- `state`: Current Raft state (Follower, Candidate, or Leader)
- `term`: Current term number
- `log_length`: Number of entries in the log
- `commit_index`: Index of highest log entry known to be committed
- `is_leader`: Whether this node is currently the leader
- `leader_id`: ID of the current leader (if known)

**Example:**
```bash
curl http://localhost:9080/status
```

**Example Response:**
```json
{
  "node_id": "node1",
  "state": "Leader",
  "term": 5,
  "log_length": 42,
  "commit_index": 41,
  "is_leader": true,
  "leader_id": "node1"
}
```

---

### 5. Leader Information

Get information about the current cluster leader.

**Endpoint:** `GET /leader`

**Success Response:**
- **Code:** 200 OK
- **Content:**
```json
{
  "leader_id": "string",
  "leader_addr": "string",
  "term": 0
}
```

**Error Responses:**

- **Code:** 503 Service Unavailable
  - No leader currently elected
  ```json
  {
    "error": "no leader elected"
  }
  ```

**Example:**
```bash
curl http://localhost:9080/leader
```

**Example Response:**
```json
{
  "leader_id": "node2",
  "leader_addr": "localhost:8081",
  "term": 5
}
```

---

## Error Handling

### Common Error Codes

| Code | Description | When it occurs |
|------|-------------|----------------|
| 400 | Bad Request | Invalid JSON, missing fields, or malformed request |
| 404 | Not Found | Key doesn't exist in the store |
| 500 | Internal Server Error | Consensus timeout, internal failure |
| 503 | Service Unavailable | Node is not leader, operation cannot proceed |

### Leader Redirection

When a write operation (PUT/DELETE) is sent to a non-leader node, the server responds with:

```json
{
  "error": "not leader",
  "leader": "node_id:address"
}
```

Clients should retry the request against the leader node.

## Client Implementation Pattern

### Example: Resilient Client with Leader Following

```go
package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"
)

type Client struct {
    nodes       []string
    currentNode int
    httpClient  *http.Client
}

func (c *Client) Put(key, value string) error {
    data := map[string]string{"key": key, "value": value}
    body, _ := json.Marshal(data)

    for attempt := 0; attempt < len(c.nodes); attempt++ {
        url := fmt.Sprintf("http://%s/kv", c.nodes[c.currentNode])
        resp, err := c.httpClient.Post(url, "application/json", bytes.NewReader(body))

        if err != nil {
            c.currentNode = (c.currentNode + 1) % len(c.nodes)
            continue
        }

        if resp.StatusCode == 503 {
            // Not leader, try next node
            c.currentNode = (c.currentNode + 1) % len(c.nodes)
            resp.Body.Close()
            continue
        }

        resp.Body.Close()
        return nil
    }

    return fmt.Errorf("failed to put after trying all nodes")
}
```

## Rate Limiting

Currently, there is no built-in rate limiting. Consider implementing application-level rate limiting or using a reverse proxy with rate limiting capabilities for production deployments.

## Security Considerations

### Current Implementation
- No authentication or authorization
- No TLS/SSL support
- Designed for trusted internal networks

### Production Recommendations
1. Deploy behind a reverse proxy (nginx, HAProxy) with TLS
2. Implement authentication at the proxy level
3. Use network segmentation and firewalls
4. Consider adding API key authentication for multi-tenant scenarios

## Versioning

The API is currently unversioned. Future versions may include versioning in the URL path (e.g., `/v1/kv`).
