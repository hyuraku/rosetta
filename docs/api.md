# API Documentation

> Last verified: 2026-09-06 against commit `d370c72`.

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

Reads are served by the leader through the ReadIndex protocol (Raft dissertation §6.4): the leader confirms it still holds leadership with a fresh heartbeat quorum, waits until its state machine has applied through the captured commit index, then returns the value from local state. A non-leader responds with the same 503 leader redirect used by writes.

> **Note:** Reads are linearizable. The earlier lease-based read path (which had confirmed linearizability violations) has been replaced by ReadIndex, so a partitioned ex-leader fails the read instead of returning a stale value. Immediately after an election a read may briefly return an error until the leader's no-op entry commits. See ../KNOWN_ISSUES.md (D1–D3, fixed).

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
- `log_size`: The absolute index of the last log entry (`raft/node.go:142-144`), **not**
  a count of entries currently held in memory. After log compaction, entries below
  the snapshot boundary are gone but `log_size` still reports the absolute index,
  so it is not "the number of entries in the log."

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

### No Batch Endpoint

`kvstore/client.go`'s `Client.Batch`/`PutBatch`/`GetBatch` send `POST /kv/batch`,
but the server registers no route for it. The request matches the `/kv/` prefix
handler instead and is treated as a plain PUT: the batch's `operations` field is
not a field of `PutArgs{Key,Value}`, so it decodes to an empty key/value and is
stored as an empty-string PUT, returning `{"success":true}`. Batch operations are
**not implemented** and silently do the wrong thing rather than failing loudly.
See ../KNOWN_ISSUES.md (R11).

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

> **Warning:** The duplicate-detection mechanism (`client_id`/`seq_num`) is wired
> into the HTTP API (D4, fixed) and a write no longer returns a spurious
> `leadership lost` after it was in fact committed (D5, fixed). But two caveats
> remain: dedup only applies when the request carries a non-empty `client_id`
> (`kvstore/store.go:386`) — a request without one gets no duplicate protection,
> so retrying it after a timeout can still apply the operation twice. And the
> committed result is matched against a per-request `opID`
> (`kvstore/store.go:583`, `<nodeID>-<UnixNano>`) registered in `pendingOps`
> *after* the entry is appended to the Raft log (`kvstore/store.go:598-614`); if
> the entry is committed and applied before that registration completes, the
> result is dropped and the client sees a timeout for an operation that in fact
> succeeded. See ../KNOWN_ISSUES.md (D4, D5, R9).

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
