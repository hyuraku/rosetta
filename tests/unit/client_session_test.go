package unit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"rosetta/kvstore"
)

// TestClientSeqNumMonotonicUnderConcurrency verifies that a single Client hands
// out strictly unique, contiguous sequence numbers even when Put is called
// concurrently, and that all requests carry the same ClientID. This is the
// invariant that makes server-side duplicate detection reliable.
func TestClientSeqNumMonotonicUnderConcurrency(t *testing.T) {
	const numRequests = 50

	var (
		mu        sync.Mutex
		seqs      []int
		clientIDs = map[string]struct{}{}
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args kvstore.PutArgs
		_ = json.NewDecoder(r.Body).Decode(&args)

		mu.Lock()
		seqs = append(seqs, args.SeqNum)
		clientIDs[args.ClientID] = struct{}{}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(kvstore.Reply{Success: true})
	}))
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	client := kvstore.NewClient([]string{host})
	defer client.Close()

	var wg sync.WaitGroup
	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := client.Put(fmt.Sprintf("k%d", i), "v"); err != nil {
				t.Errorf("Put failed: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if len(seqs) != numRequests {
		t.Fatalf("expected %d requests, got %d", numRequests, len(seqs))
	}

	seen := make(map[int]bool, numRequests)
	for _, s := range seqs {
		if s < 1 || s > numRequests {
			t.Errorf("sequence number %d out of expected range [1,%d]", s, numRequests)
		}
		if seen[s] {
			t.Errorf("duplicate sequence number %d handed out", s)
		}
		seen[s] = true
	}

	if len(clientIDs) != 1 {
		t.Errorf("expected all requests to share one ClientID, got %d distinct", len(clientIDs))
	}
	for id := range clientIDs {
		if id == "" {
			t.Error("client sent an empty ClientID")
		}
	}
}

// TestClientRetryReusesSeqNum verifies that when the client transparently
// retries against another server (e.g. after a 503 redirect), it reuses the
// SAME sequence number rather than allocating a new one. Reusing the SeqNum is
// what lets the server deduplicate the retried write.
func TestClientRetryReusesSeqNum(t *testing.T) {
	var (
		mu         sync.Mutex
		firstSeqs  []int
		secondSeqs []int
	)

	// First server always responds 503 (not leader), forcing a retry.
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args kvstore.PutArgs
		_ = json.NewDecoder(r.Body).Decode(&args)
		mu.Lock()
		firstSeqs = append(firstSeqs, args.SeqNum)
		mu.Unlock()
		http.Error(w, "not leader", http.StatusServiceUnavailable)
	}))
	defer first.Close()

	// Second server accepts the write.
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var args kvstore.PutArgs
		_ = json.NewDecoder(r.Body).Decode(&args)
		mu.Lock()
		secondSeqs = append(secondSeqs, args.SeqNum)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(kvstore.Reply{Success: true})
	}))
	defer second.Close()

	firstHost := strings.TrimPrefix(first.URL, "http://")
	secondHost := strings.TrimPrefix(second.URL, "http://")
	client := kvstore.NewClient([]string{firstHost, secondHost})
	defer client.Close()

	if err := client.Put("k", "v"); err != nil {
		t.Fatalf("Put should succeed after retry, got %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(firstSeqs) != 1 || len(secondSeqs) != 1 {
		t.Fatalf("expected one request to each server, got first=%v second=%v", firstSeqs, secondSeqs)
	}
	if firstSeqs[0] != secondSeqs[0] {
		t.Errorf("retry must reuse the same SeqNum: first=%d second=%d", firstSeqs[0], secondSeqs[0])
	}
}
