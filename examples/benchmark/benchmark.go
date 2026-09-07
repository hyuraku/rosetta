package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultOps            = 1000
	defaultConcurrency    = 10
	defaultReadRatio      = 0.5
	defaultKeySize        = 16
	defaultValueSize      = 100
	defaultReportInterval = 1000

	clientTimeout        = 30 * time.Second
	idleConnTimeout      = 90 * time.Second
	idleConnMultiplier   = 2
	httpErrorThreshold   = 400
	progressTickInterval = 2 * time.Second
	percentMultiplier    = 100
	// populateFraction is the denominator used to size the preloaded read
	// pool from -ops: 1/populateFraction of -ops keys are written before the
	// timed run starts, then only reads pick indices from that pool.
	populateFraction = 10
)

type Config struct {
	URL            string
	Operations     int
	Concurrency    int
	ReadRatio      float64
	KeySize        int
	ValueSize      int
	Duration       int
	ReportInterval int
}

// Stats accumulates results across every worker goroutine. All counters are
// updated with atomic operations (the mu-guarded slices are the exception,
// appended to under Add*Latency) so workers never contend on a shared lock
// per operation.
type Stats struct {
	TotalOps       int64
	SuccessOps     int64
	FailedOps      int64
	WriteLatencies []time.Duration
	ReadLatencies  []time.Duration
	StartTime      time.Time
	EndTime        time.Time
	mu             sync.Mutex

	// Status breakdown (KNOWN_ISSUES.md R18): every completed request is
	// counted in exactly one of these buckets, classified in recordStatus. A
	// read that gets a 404 is counted here as a failure (Status404, and
	// FailedOps) — the whole point of a deterministic key is that a read is
	// supposed to hit.
	Status2xx       int64
	Status404       int64
	Status503       int64
	StatusOther4xx  int64
	Status5xx       int64
	TransportErrors int64 // no HTTP response at all: dial/timeout/context errors

	// writeIndexCounter hands out the index each write uses, starting just
	// after the preloaded range so writes never collide with a key a read
	// might also pick (see runBenchmark/worker).
	writeIndexCounter int64
}

func (s *Stats) AddWriteLatency(d time.Duration) {
	s.mu.Lock()
	s.WriteLatencies = append(s.WriteLatencies, d)
	s.mu.Unlock()
}

func (s *Stats) AddReadLatency(d time.Duration) {
	s.mu.Lock()
	s.ReadLatencies = append(s.ReadLatencies, d)
	s.mu.Unlock()
}

// recordStatus classifies one completed request into the status breakdown.
// statusCode is 0 when no HTTP response was received at all — put/get return
// 0 exactly for a transport-level error (dial failure, timeout, canceled
// context), and a real status code otherwise, whether or not it also produced
// an error.
func (s *Stats) recordStatus(statusCode int) {
	switch {
	case statusCode == 0:
		atomic.AddInt64(&s.TransportErrors, 1)
	case statusCode >= 200 && statusCode < 300:
		atomic.AddInt64(&s.Status2xx, 1)
	case statusCode == http.StatusNotFound:
		atomic.AddInt64(&s.Status404, 1)
	case statusCode == http.StatusServiceUnavailable:
		atomic.AddInt64(&s.Status503, 1)
	case statusCode >= 400 && statusCode < 500:
		atomic.AddInt64(&s.StatusOther4xx, 1)
	case statusCode >= 500:
		atomic.AddInt64(&s.Status5xx, 1)
	default:
		// Should not happen (every branch above is exhaustive for a valid HTTP
		// status), but classify defensively rather than dropping the sample.
		atomic.AddInt64(&s.TransportErrors, 1)
	}
}

func main() {
	config := parseFlags()

	if err := validateConfig(config); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n\n", err)
		flag.Usage()
		os.Exit(2)
	}

	fmt.Println("=== Rosetta Benchmark ===")
	fmt.Printf("URL: %s\n", config.URL)
	if config.Duration > 0 {
		fmt.Printf("Duration: %ds\n", config.Duration)
	} else {
		fmt.Printf("Operations: %d\n", config.Operations)
	}
	fmt.Printf("Concurrency: %d\n", config.Concurrency)
	fmt.Printf("Read Ratio: %.2f\n", config.ReadRatio)
	fmt.Printf("Key Size: %d bytes\n", config.KeySize)
	fmt.Printf("Value Size: %d bytes\n", config.ValueSize)
	fmt.Println()

	stats := runBenchmark(config)
	printResults(stats)
}

func parseFlags() *Config {
	config := &Config{}

	flag.StringVar(&config.URL, "url", "http://localhost:9080", "Rosetta node URL")
	flag.IntVar(&config.Operations, "ops", defaultOps, "Total number of operations")
	flag.IntVar(&config.Concurrency, "concurrency", defaultConcurrency, "Number of concurrent clients")
	flag.Float64Var(&config.ReadRatio, "read-ratio", defaultReadRatio, "Ratio of read operations (0.0 to 1.0)")
	flag.IntVar(&config.KeySize, "key-size", defaultKeySize, "Size of keys in bytes")
	flag.IntVar(&config.ValueSize, "value-size", defaultValueSize, "Size of values in bytes")
	flag.IntVar(&config.Duration, "duration", 0, "Benchmark duration in seconds (0 = use ops count)")
	flag.IntVar(&config.ReportInterval, "report-interval", defaultReportInterval, "Progress report interval in ops")

	flag.Parse()
	return config
}

// preloadCount is how many keys populateInitialData writes before the timed
// run starts: 1/populateFraction of -ops, floored at 1 so a small -ops value
// (e.g. -ops=5, which is < populateFraction) still leaves at least one key
// for reads to pick — config.Operations/populateFraction alone can be 0,
// which would make worker's r.Intn(0) panic.
func preloadCount(ops int) int {
	n := ops / populateFraction
	if n < 1 {
		n = 1
	}
	return n
}

// maxPlannedIndex bounds the largest index keyForIndex is expected to see for
// an ops-based run (config.Duration == 0): every preloaded key, plus every
// operation being a write in the worst case. validateConfig uses it to size
// -key-size. A duration-based run (config.Duration > 0) has no fixed op
// count — it runs until the clock, not -ops, says stop — so this bound is
// only a best-effort default for the flag's row in printed help; an index
// past it is not an error, it just truncates (see keyForIndex) and risks a
// collision between two indices.
func maxPlannedIndex(config *Config) int {
	preload := 0
	if config.ReadRatio > 0 {
		preload = preloadCount(config.Operations)
	}
	return preload + config.Operations
}

// validateConfig rejects argument combinations that would otherwise fail
// confusingly deep inside the run (a panic, a benchmark that silently never
// exercises reads, or a report with no numbers), per KNOWN_ISSUES.md R18.
func validateConfig(config *Config) error {
	switch {
	case config.Operations <= 0:
		return fmt.Errorf("-ops must be > 0, got %d", config.Operations)
	case config.Concurrency <= 0:
		return fmt.Errorf("-concurrency must be > 0, got %d", config.Concurrency)
	case config.ReadRatio < 0 || config.ReadRatio > 1:
		return fmt.Errorf("-read-ratio must be within [0, 1], got %v", config.ReadRatio)
	case config.ValueSize <= 0:
		return fmt.Errorf("-value-size must be > 0, got %d", config.ValueSize)
	case config.ReportInterval <= 0:
		return fmt.Errorf("-report-interval must be > 0, got %d", config.ReportInterval)
	case config.Duration < 0:
		return fmt.Errorf("-duration must be >= 0, got %d", config.Duration)
	}

	if required := minKeySize(maxPlannedIndex(config)); config.KeySize < required {
		return fmt.Errorf(
			"-key-size must be at least %d for -ops=%d (prefix %q plus index digits and separator), got %d",
			required, config.Operations, keyPrefix, config.KeySize)
	}

	return nil
}

func runBenchmark(config *Config) *Stats {
	stats := &Stats{
		WriteLatencies: make([]time.Duration, 0),
		ReadLatencies:  make([]time.Duration, 0),
		StartTime:      time.Now(),
	}

	client := &http.Client{
		Timeout: clientTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        config.Concurrency * idleConnMultiplier,
			MaxIdleConnsPerHost: config.Concurrency * idleConnMultiplier,
			IdleConnTimeout:     idleConnTimeout,
		},
	}

	fmt.Println("Running benchmark...")

	var wg sync.WaitGroup
	workChan := make(chan bool, config.Operations)

	// Start progress reporter
	stopReporter := make(chan bool)
	go progressReporter(stats, config, stopReporter)

	// Populate initial data for reads. preloaded is how many keys are
	// available to read: worker picks its read index from [0, preloaded), and
	// writes are indexed starting just past it, so a write from this run can
	// never overwrite (and a read can never accidentally hit) a key outside
	// the range preload actually wrote (KNOWN_ISSUES.md R18).
	preloaded := 0
	if config.ReadRatio > 0 {
		preloaded = populateInitialData(client, config)
	}

	// Start workers
	for i := 0; i < config.Concurrency; i++ {
		wg.Add(1)
		go worker(i, client, config, stats, workChan, &wg, preloaded)
	}

	dispatchWork(config, workChan)

	close(workChan)
	wg.Wait()
	stopReporter <- true

	stats.EndTime = time.Now()
	return stats
}

// populateInitialData writes preloadCount(config.Operations) keys with
// deterministic, index-derived keys (keyForIndex) so the read workload has a
// known set of keys to pick from. It returns how many keys were written
// successfully — worker only ever picks read indices below that count, so a
// preload failure shrinks the read pool instead of producing a read that was
// never going to hit.
func populateInitialData(client *http.Client, config *Config) int {
	fmt.Println("Populating initial data...")
	// #nosec G404 -- benchmark payload generation, not security-sensitive
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	count := preloadCount(config.Operations)
	written := 0
	failures := 0
	for i := 0; i < count; i++ {
		key := keyForIndex(i, config.KeySize)
		value := generateValue(r, config.ValueSize)
		if _, _, err := put(client, config.URL, key, value); err != nil {
			failures++
			continue
		}
		written++
	}

	if failures > 0 {
		fmt.Printf("Initial data populated (%d/%d keys; %d preload writes failed)\n", written, count, failures)
	} else {
		fmt.Printf("Initial data populated (%d keys)\n", written)
	}
	// written, not count: a read must only ever pick an index this preload
	// pass actually confirmed as written, or it inherits the very
	// preload/read mismatch this fix exists to close.
	return written
}

func dispatchWork(config *Config, workChan chan<- bool) {
	if config.Duration > 0 {
		// Time-based benchmark
		timeout := time.After(time.Duration(config.Duration) * time.Second)
		for {
			select {
			case <-timeout:
				return
			default:
				workChan <- true
			}
		}
	}

	// Operation-based benchmark
	for i := 0; i < config.Operations; i++ {
		workChan <- true
	}
}

// worker runs one client loop, issuing reads and writes with keyForIndex
// keys. preloaded is the number of keys populateInitialData confirmed
// written: reads pick their index uniformly from [0, preloaded), and writes
// are indexed starting at preloaded so they can never collide with (or,
// mid-run, overwrite) a key a read might pick (KNOWN_ISSUES.md R18).
func worker(
	id int, client *http.Client, config *Config, stats *Stats, workChan <-chan bool, wg *sync.WaitGroup, preloaded int,
) {
	defer wg.Done()

	// #nosec G404 -- benchmark payload generation, not security-sensitive
	r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))

	for range workChan {
		atomic.AddInt64(&stats.TotalOps, 1)

		// Decide operation type. Reads only happen when there is a preloaded
		// pool to read from — config.ReadRatio > 0 implies preloaded > 0 (see
		// runBenchmark and preloadCount's floor), so this is a defensive
		// fallback, not the normal path.
		isRead := preloaded > 0 && r.Float64() < config.ReadRatio

		var (
			err        error
			latency    time.Duration
			statusCode int
		)

		if isRead {
			index := r.Intn(preloaded)
			key := keyForIndex(index, config.KeySize)
			latency, statusCode, err = get(client, config.URL, key)
			if err == nil {
				stats.AddReadLatency(latency)
			}
		} else {
			index := preloaded + int(atomic.AddInt64(&stats.writeIndexCounter, 1)) - 1
			key := keyForIndex(index, config.KeySize)
			value := generateValue(r, config.ValueSize)
			latency, statusCode, err = put(client, config.URL, key, value)
			if err == nil {
				stats.AddWriteLatency(latency)
			}
		}

		stats.recordStatus(statusCode)

		if err != nil {
			atomic.AddInt64(&stats.FailedOps, 1)
		} else {
			atomic.AddInt64(&stats.SuccessOps, 1)
		}
	}
}

// put issues one PUT and returns its latency, HTTP status code (0 if no
// response was received at all), and an error for any status >=
// httpErrorThreshold or a transport-level failure.
func put(client *http.Client, baseURL, key, value string) (time.Duration, int, error) {
	data := map[string]string{
		"key":   key,
		"value": value,
	}

	body, err := json.Marshal(data)
	if err != nil {
		return 0, 0, err
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/kv", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	// #nosec G704 -- target URL is an intentional CLI benchmark argument, not untrusted input
	resp, err := client.Do(req)
	latency := time.Since(start)

	if err != nil {
		return latency, 0, err
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= httpErrorThreshold {
		return latency, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return latency, resp.StatusCode, nil
}

// get issues one GET and returns its latency, HTTP status code (0 if no
// response was received at all), and an error for any status >=
// httpErrorThreshold or a transport-level failure.
func get(client *http.Client, baseURL, key string) (time.Duration, int, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/kv/"+key, http.NoBody)
	if err != nil {
		return 0, 0, err
	}

	start := time.Now()
	// #nosec G704 -- target URL is an intentional CLI benchmark argument, not untrusted input
	resp, err := client.Do(req)
	latency := time.Since(start)

	if err != nil {
		return latency, 0, err
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= httpErrorThreshold {
		return latency, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return latency, resp.StatusCode, nil
}

func generateValue(r *rand.Rand, size int) string {
	return randomString(r, size)
}

func randomString(r *rand.Rand, n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[r.Intn(len(letters))]
	}
	return string(b)
}

func progressReporter(stats *Stats, config *Config, stop <-chan bool) {
	ticker := time.NewTicker(progressTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			total := atomic.LoadInt64(&stats.TotalOps)
			elapsed := time.Since(stats.StartTime).Seconds()

			if config.Duration > 0 {
				fmt.Printf("Progress: %d ops - Elapsed: %.1fs - Rate: %.0f ops/sec\n",
					total, elapsed, float64(total)/elapsed)
			} else {
				pct := float64(total) / float64(config.Operations) * percentMultiplier
				fmt.Printf("Progress: %d/%d ops (%.1f%%) - Elapsed: %.1fs\n",
					total, config.Operations, pct, elapsed)
			}
		case <-stop:
			return
		}
	}
}

func printResults(stats *Stats) {
	duration := stats.EndTime.Sub(stats.StartTime)

	fmt.Println("\n=== Results ===")
	fmt.Printf("Total Operations: %d\n", stats.TotalOps)
	fmt.Printf("Successful: %d\n", stats.SuccessOps)
	fmt.Printf("Failed: %d\n", stats.FailedOps)
	if stats.TotalOps > 0 {
		successRate := float64(stats.SuccessOps) / float64(stats.TotalOps) * percentMultiplier
		fmt.Printf("Success Rate: %.2f%%\n", successRate)
	}
	fmt.Printf("Duration: %.1fs\n", duration.Seconds())

	throughput := float64(stats.SuccessOps) / duration.Seconds()
	fmt.Printf("Throughput: %.0f ops/sec\n", throughput)

	if len(stats.WriteLatencies) > 0 {
		fmt.Println("\nWrite Latency:")
		printLatencyStats(stats.WriteLatencies)
	}

	if len(stats.ReadLatencies) > 0 {
		fmt.Println("\nRead Latency:")
		printLatencyStats(stats.ReadLatencies)
	}

	fmt.Println("\nStatus Breakdown:")
	fmt.Printf("  2xx:                       %d\n", stats.Status2xx)
	fmt.Printf("  404 Not Found:             %d\n", stats.Status404)
	fmt.Printf("  503 Service Unavailable:   %d\n", stats.Status503)
	fmt.Printf("  Other 4xx:                 %d\n", stats.StatusOther4xx)
	fmt.Printf("  5xx:                       %d\n", stats.Status5xx)
	fmt.Printf("  Transport errors/timeouts: %d\n", stats.TransportErrors)

	if stats.FailedOps > 0 {
		errorRate := float64(stats.FailedOps) / float64(stats.TotalOps) * percentMultiplier
		fmt.Printf("\nError Rate: %.2f%%\n", errorRate)
	}
}

func printLatencyStats(latencies []time.Duration) {
	if len(latencies) == 0 {
		return
	}

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	minLatency := latencies[0]
	maxLatency := latencies[len(latencies)-1]

	var sum time.Duration
	for _, l := range latencies {
		sum += l
	}
	mean := sum / time.Duration(len(latencies))

	p50 := latencies[len(latencies)*50/100]
	p95 := latencies[len(latencies)*95/100]
	p99 := latencies[len(latencies)*99/100]

	fmt.Printf("  Min: %v\n", minLatency)
	fmt.Printf("  Max: %v\n", maxLatency)
	fmt.Printf("  Mean: %v\n", mean)
	fmt.Printf("  P50: %v\n", p50)
	fmt.Printf("  P95: %v\n", p95)
	fmt.Printf("  P99: %v\n", p99)
}
