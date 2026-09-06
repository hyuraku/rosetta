package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rosetta/config"
	"rosetta/kvstore"
	"rosetta/raft"
)

// newTestHTTPServer builds an HTTPServer backed by a real single-node Raft
// cluster (MockTransport, no network), waiting for it to win its election so
// writes are accepted. Tests call hs.handleKV/handlePut directly against an
// httptest.ResponseRecorder rather than starting hs.server, so no port is ever
// bound.
func newTestHTTPServer(t *testing.T) *HTTPServer {
	t.Helper()

	kvs := kvstore.NewKVStore(1000)
	transport := raft.NewMockTransport()
	node := raft.NewRaftNode("node1", []string{"node1"}, transport, kvs.GetApplyCh())
	transport.RegisterNode("node1", node)
	kvs.SetRaft(node)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if node.IsLeader() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !node.IsLeader() {
		t.Fatal("node did not become leader")
	}

	cfg := config.DefaultConfig()
	cfg.NodeID = "node1"

	hs := NewHTTPServer(kvs, node, cfg)
	t.Cleanup(func() {
		kvs.Close()
		node.Kill()
	})
	return hs
}

// TestHandleKVBatchPathReturns501 verifies the R11 fix: a request to
// /kv/batch, which has no registered route, is rejected explicitly with 501
// and a JSON body instead of falling through to handlePut and being silently
// misread as a successful empty-key PUT.
func TestHandleKVBatchPathReturns501(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			hs := newTestHTTPServer(t)

			req := httptest.NewRequest(method, kvBatchPath, strings.NewReader(`{"operations":[]}`))
			rec := httptest.NewRecorder()

			hs.handleKV(rec, req)

			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("expected 501, got %d: %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("expected JSON content type, got %q", ct)
			}

			var reply map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
				t.Fatalf("failed to decode JSON body %q: %v", rec.Body.String(), err)
			}
			if success, _ := reply["success"].(bool); success {
				t.Error("expected success:false in the batch rejection body")
			}
			if reply["error"] != "batch operations are not implemented" {
				t.Errorf("unexpected error message: %v", reply["error"])
			}
		})
	}
}

// TestHandlePutRejectsEmptyKey verifies the R11 fix's other half: an empty-key
// PUT -- the exact shape a misrouted batch request decodes to -- is now a 400,
// not a silent success.
func TestHandlePutRejectsEmptyKey(t *testing.T) {
	hs := newTestHTTPServer(t)

	req := httptest.NewRequest(http.MethodPut, "/kv", strings.NewReader(`{"key":"","value":"x"}`))
	rec := httptest.NewRecorder()

	hs.handleKV(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an empty key, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandlePutAcceptsNonEmptyKey is a regression guard alongside the empty-key
// rejection above: a normal PUT must still succeed.
func TestHandlePutAcceptsNonEmptyKey(t *testing.T) {
	hs := newTestHTTPServer(t)

	req := httptest.NewRequest(http.MethodPut, "/kv", strings.NewReader(`{"key":"k","value":"v"}`))
	rec := httptest.NewRecorder()

	hs.handleKV(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
