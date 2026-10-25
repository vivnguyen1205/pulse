// loadgen simulates many agents sending metrics to a Pulse server at once,
// and reports throughput, latency, and errors.
//
// Example: go run . -agents 500 -duration 30s
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
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Metric struct {
	Host      string  `json:"host"`
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	Timestamp int64   `json:"timestamp"`
}

// Fake metric names each simulated agent reports.
var metricNames = []string{
	"cpu.percent", "mem.percent", "disk.percent", "net.rx_kbps", "net.tx_kbps",
	"load.1m", "proc.count", "temp.celsius", "io.read_kbps", "io.write_kbps",
}

// stats is shared by all simulated agents, so it must be safe for concurrent use.
type stats struct {
	requests atomic.Int64
	failures atomic.Int64
	points   atomic.Int64

	mu        sync.Mutex
	latencies []time.Duration
	errors    map[string]int
}

func (s *stats) record(latency time.Duration, n int, err error) {
	s.requests.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.failures.Add(1)
		s.errors[err.Error()]++
		return
	}
	s.points.Add(int64(n))
	s.latencies = append(s.latencies, latency)
}

func main() {
	url := flag.String("url", "http://localhost:8080", "server address")
	key := flag.String("key", os.Getenv("PULSE_API_KEY"), "API key (defaults to $PULSE_API_KEY)")
	agents := flag.Int("agents", 100, "number of simulated agents")
	interval := flag.Duration("interval", time.Second, "how often each agent sends")
	perReq := flag.Int("metrics", 10, "metrics per request (1-10)")
	duration := flag.Duration("duration", 30*time.Second, "how long to run")
	flag.Parse()

	if *perReq < 1 || *perReq > len(metricNames) {
		fmt.Fprintf(os.Stderr, "-metrics must be between 1 and %d\n", len(metricNames))
		os.Exit(1)
	}

	// Go's default client keeps only 2 idle connections per host. With hundreds of
	// agents that means constantly opening new connections, which would make the
	// load generator itself the bottleneck. Allow one connection per agent.
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        *agents,
			MaxIdleConnsPerHost: *agents,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Stop after -duration, or earlier on Ctrl+C.
	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()

	target := float64(*agents**perReq) / interval.Seconds()
	fmt.Printf("Simulating %d agents × %d metrics every %s → target %.0f points/sec for %s\n\n",
		*agents, *perReq, *interval, target, *duration)

	s := &stats{errors: map[string]int{}}
	var wg sync.WaitGroup
	start := time.Now()

	for i := 0; i < *agents; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runAgent(ctx, id, client, *url, *key, *interval, *perReq, s)
		}(i)
	}

	go reportProgress(ctx, s, start)

	wg.Wait()
	printSummary(s, time.Since(start), target)
}

// runAgent behaves like one real agent: send a batch, wait, repeat.
func runAgent(ctx context.Context, id int, client *http.Client, url, key string,
	interval time.Duration, n int, s *stats) {

	host := fmt.Sprintf("sim-%04d", id)
	rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))

	values := make([]float64, n)
	for i := range values {
		values[i] = 10 + rng.Float64()*50
	}

	// Start at a random moment within the first interval, so agents don't all
	// fire at exactly the same instant (real servers don't either).
	select {
	case <-time.After(time.Duration(rng.Int63n(int64(interval)))):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		now := time.Now().Unix()
		batch := make([]Metric, n)
		for j := range batch {
			// Random walk: each value drifts a little from its last value.
			values[j] += rng.NormFloat64() * 2
			values[j] = max(0, min(100, values[j]))
			batch[j] = Metric{Host: host, Name: metricNames[j], Value: values[j], Timestamp: now}
		}

		t0 := time.Now()
		err := send(ctx, client, url, key, batch)
		if ctx.Err() == nil { // ignore requests cut off by the test ending
			s.record(time.Since(t0), n, err)
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return
		}
	}
}

func send(ctx context.Context, client *http.Client, url, key string, batch []Metric) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/metrics", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed (timeout or connection error)")
	}
	// Read the body fully so the connection can be reused.
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	return nil
}

// reportProgress prints a live line every second.
func reportProgress(ctx context.Context, s *stats, start time.Time) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var prevPoints int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			points := s.points.Load()
			fmt.Printf("t=%3.0fs  %7d points/sec  requests=%d  failed=%d\n",
				time.Since(start).Seconds(), points-prevPoints, s.requests.Load(), s.failures.Load())
			prevPoints = points
		}
	}
}

func printSummary(s *stats, elapsed time.Duration, target float64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	lat := s.latencies
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })

	actual := float64(s.points.Load()) / elapsed.Seconds()

	fmt.Println("\n========== RESULTS ==========")
	fmt.Printf("Duration:          %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Requests:          %d (%d failed)\n", s.requests.Load(), s.failures.Load())
	fmt.Printf("Points stored:     %d\n", s.points.Load())
	fmt.Printf("Throughput:        %.0f points/sec (target %.0f, %.0f%%)\n", actual, target, 100*actual/target)
	if len(lat) > 0 {
		fmt.Printf("Latency p50:       %s\n", percentile(lat, 0.50))
		fmt.Printf("Latency p95:       %s\n", percentile(lat, 0.95))
		fmt.Printf("Latency p99:       %s\n", percentile(lat, 0.99))
		fmt.Printf("Latency max:       %s\n", lat[len(lat)-1].Round(time.Millisecond))
	}
	if len(s.errors) > 0 {
		fmt.Println("Errors:")
		for msg, count := range s.errors {
			fmt.Printf("  %6d × %s\n", count, msg)
		}
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	i := int(float64(len(sorted)) * p)
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i].Round(100 * time.Microsecond)
}