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
	keyPrefixOverhead    = 10
	progressTickInterval = 2 * time.Second
	percentMultiplier    = 100
	populateFraction     = 10
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

type Stats struct {
	TotalOps       int64
	SuccessOps     int64
	FailedOps      int64
	WriteLatencies []time.Duration
	ReadLatencies  []time.Duration
	StartTime      time.Time
	EndTime        time.Time
	mu             sync.Mutex
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

func main() {
	config := parseFlags()

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

	// Populate initial data for reads
	if config.ReadRatio > 0 {
		populateInitialData(client, config)
	}

	// Start workers
	for i := 0; i < config.Concurrency; i++ {
		wg.Add(1)
		go worker(i, client, config, stats, workChan, &wg)
	}

	dispatchWork(config, workChan)

	close(workChan)
	wg.Wait()
	stopReporter <- true

	stats.EndTime = time.Now()
	return stats
}

func populateInitialData(client *http.Client, config *Config) {
	fmt.Println("Populating initial data...")
	// #nosec G404 -- benchmark payload generation, not security-sensitive
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < config.Operations/populateFraction; i++ {
		key := generateKey(r, config.KeySize, i)
		value := generateValue(r, config.ValueSize)
		_, _ = put(client, config.URL, key, value)
	}
	fmt.Println("Initial data populated")
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

func worker(id int, client *http.Client, config *Config, stats *Stats, workChan <-chan bool, wg *sync.WaitGroup) {
	defer wg.Done()

	// #nosec G404 -- benchmark payload generation, not security-sensitive
	r := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))

	for range workChan {
		atomic.AddInt64(&stats.TotalOps, 1)

		// Decide operation type
		isRead := r.Float64() < config.ReadRatio

		var err error
		var latency time.Duration

		if isRead {
			// Read operation
			key := generateKey(r, config.KeySize, r.Intn(config.Operations/populateFraction))
			latency, err = get(client, config.URL, key)
			if err == nil {
				stats.AddReadLatency(latency)
			}
		} else {
			// Write operation
			key := generateKey(r, config.KeySize, int(atomic.LoadInt64(&stats.TotalOps)))
			value := generateValue(r, config.ValueSize)
			latency, err = put(client, config.URL, key, value)
			if err == nil {
				stats.AddWriteLatency(latency)
			}
		}

		if err != nil {
			atomic.AddInt64(&stats.FailedOps, 1)
		} else {
			atomic.AddInt64(&stats.SuccessOps, 1)
		}
	}
}

func put(client *http.Client, baseURL, key, value string) (time.Duration, error) {
	data := map[string]string{
		"key":   key,
		"value": value,
	}

	body, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+"/kv", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	// #nosec G704 -- target URL is an intentional CLI benchmark argument, not untrusted input
	resp, err := client.Do(req)
	latency := time.Since(start)

	if err != nil {
		return latency, err
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= httpErrorThreshold {
		return latency, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return latency, nil
}

func get(client *http.Client, baseURL, key string) (time.Duration, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/kv/"+key, http.NoBody)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	// #nosec G704 -- target URL is an intentional CLI benchmark argument, not untrusted input
	resp, err := client.Do(req)
	latency := time.Since(start)

	if err != nil {
		return latency, err
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= httpErrorThreshold {
		return latency, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return latency, nil
}

func generateKey(r *rand.Rand, size, seed int) string {
	return fmt.Sprintf("key-%d-%s", seed, randomString(r, size-keyPrefixOverhead))
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
