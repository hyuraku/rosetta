package unit

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"rosetta/kvstore"
)

// TestClientBatchNotImplementedSendsNoRequest verifies the client-side half of
// the R11 fix: Batch, PutBatch, and GetBatch reject locally with
// ErrBatchNotImplemented and never send an HTTP request, rather than posting to
// a "/kv/batch" route the server has never had.
func TestClientBatchNotImplementedSendsNoRequest(t *testing.T) {
	var requestCount int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	client := kvstore.NewClient([]string{host})
	defer client.Close()

	if _, err := client.Batch(nil); !errors.Is(err, kvstore.ErrBatchNotImplemented) {
		t.Errorf("Batch: expected ErrBatchNotImplemented, got %v", err)
	}
	if err := client.PutBatch(map[string]string{"k": "v"}); !errors.Is(err, kvstore.ErrBatchNotImplemented) {
		t.Errorf("PutBatch: expected ErrBatchNotImplemented, got %v", err)
	}
	if _, err := client.GetBatch([]string{"k"}); !errors.Is(err, kvstore.ErrBatchNotImplemented) {
		t.Errorf("GetBatch: expected ErrBatchNotImplemented, got %v", err)
	}

	if got := atomic.LoadInt32(&requestCount); got != 0 {
		t.Fatalf("expected no HTTP requests to be sent, server received %d", got)
	}
}
