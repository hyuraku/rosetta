# API Documentation

> Last verified: 2026-07-21 against commit `9383cfe`.

This document provides detailed information about the Rosetta HTTP API.

## Base URL

```
http://<node-address>:<http-port>
```

Example: `http://localhost:9080`

## Response Formats

- **Success responses** are JSON (`Content-Type: application/json`).
- **Error responses** are plain text (`Content-Type: text/plain; charset=utf-8`), produced by Go's `http.Error`. They are **not** JSON.

## Endpoints

### 1. Store Key-Value Pair

Store or update a key-value pair in the distributed store.

**Endpoint:** `PUT /kv` (POST is also accepted)

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
  "success": true
}
```

**Error Responses:**

- **Code:** 400 Bad Request
  - Invalid JSON in the request body. Missing fields are **not** validated; an empty body field is stored as an empty string.
  - Body: plain-text JSON decode error message

- **Code:** 503 Service Unavailable
  - Node is not the leader
  - Body: `Not leader. Current leader: <leader-id>`
  - Header: `X-Raft-Leader: <leader-id>` (empty if no leader is known)

- **Code:** 500 Internal Server Error
  - Body: plain-text error, e.g. `operation timeout` (commit did not complete within 5s) or `leadership lost`

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

Reads are served by the leader. If the leader's lease-based read optimization applies, the value is returned from local state without going through the Raft log; otherwise the read is submitted through Raft.

> **Warning:** The lease-based read path has known linearizability violations (lease period misuse and missing no-op entry on election), so stale reads are possible. See ../KNOWN_ISSUES.md (D1, D3).

**Success Response:**
- **Code:** 200 OK
- **Content:**
```json
{
  "success": true,
  "value": "string"
}
```

**Error Responses:**

- **Code:** 400 Bad Request
  - No key in the URL path
  - Body: `Key required`

- **Code:** 404 Not Found
  - Key does not exist
  - Body: `Key not found`

- **Code:** 503 Service Unavailable
  - Node is not the leader (reads on followers are not redirected locally; they fail with the leader hint)
  - Body: `Not leader. Current leader: <leader-id>`
  - Header: `X-Raft-Leader: <leader-id>`

- **Code:** 500 Internal Server Error
  - Body: plain-text error, e.g. `operation timeout`

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
  "success": true
}
```

Deleting a key that does not exist also returns 200; there is no 404 for DELETE.

**Error Responses:**

- **Code:** 400 Bad Request
  - No key in the URL path
  - Body: `Key required`

- **Code:** 503 Service Unavailable
  - Node is not the leader
  - Body: `Not leader. Current leader: <leader-id>`
  - Header: `X-Raft-Leader: <leader-id>`

- **Code:** 500 Internal Server Error
  - Body: plain-text error, e.g. `operation timeout`

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
  "term": 0,
  "is_leader": false,
  "log_size": 0
}
```

**Field Descriptions:**
- `node_id`: Unique identifier for this node
- `term`: Current term number
- `is_leader`: Whether this node is currently the leader
- `log_size`: Index of the last log entry (number of entries in the log)

Fields such as `state`, `commit_index`, or `leader_id` are not exposed by this endpoint.

**Example:**
```bash
curl http://localhost:9080/status
```

**Example Response:**
```json
{
  "node_id": "node1",
  "term": 5,
  "is_leader": true,
  "log_size": 42
}
```

---

### 5. Leader Information

Get the ID of the current cluster leader.

**Endpoint:** `GET /leader`

**Success Response:**
- **Code:** 200 OK
- **Content:**
```json
{
  "leader": "string"
}
```

`leader` is the node ID of the current leader (e.g. `"node2"`), not an address. If no leader is known, `leader` is an empty string — the endpoint still returns 200, never 503.

**Example:**
```bash
curl http://localhost:9080/leader
```

**Example Response:**
```json
{
  "leader": "node2"
}
```

---

## Error Handling

### Common Error Codes

| Code | Description | When it occurs |
|------|-------------|----------------|
| 400 | Bad Request | Invalid JSON body (PUT), or missing key in the URL path (GET/DELETE) |
| 404 | Not Found | Key doesn't exist (GET only) |
| 405 | Method Not Allowed | Unsupported HTTP method on `/kv` |
| 500 | Internal Server Error | Operation timeout (5s), leadership lost, internal failure |
| 503 | Service Unavailable | Node is not leader |

All error bodies are plain text, not JSON.

### Leader Redirection

When an operation is sent to a non-leader node, the server responds with status 503, the header `X-Raft-Leader: <leader-id>`, and a plain-text body:

```
Not leader. Current leader: <leader-id>
```

The leader is identified by its node ID only; clients must map node IDs to HTTP addresses themselves. Clients should retry the request against the leader node.

> **Warning:** Retrying a write after a timeout or leader change can apply the same operation twice — the duplicate-detection mechanism exists in the store but is not wired into the HTTP API, and a spurious `leadership lost` error can be returned after a write was in fact committed. See ../KNOWN_ISSUES.md (D4, D5).

## Client Implementation Pattern

### Example: Resilient Client with Leader Following

```go
package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "net/http"
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

Note: `POST /kv` works because the server accepts both PUT and POST for writes. Be aware of the double-apply caveat above when adding retries.

## Rate Limiting

There is no built-in rate limiting.

## Security Considerations

- No authentication or authorization
- No TLS/SSL support

This is a learning-purpose implementation and is not intended for production use. Do not expose the API to untrusted networks.

## Versioning

The API is unversioned.
