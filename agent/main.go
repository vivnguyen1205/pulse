package main
 
import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
 
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)
 
type Metric struct {
	Host      string  `json:"host"`
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	Timestamp int64   `json:"timestamp"`
}
 
// One shared HTTP client, with a timeout so a hung server can't freeze the agent.
var client = &http.Client{Timeout: 5 * time.Second}
 
func main() {
	host, err := os.Hostname()
	if err != nil {
		log.Fatalf("could not get hostname: %v", err)
	}
 
	serverURL := os.Getenv("PULSE_SERVER")
	if serverURL == "" {
		serverURL = "http://localhost:8080"
	}
	apiKey := os.Getenv("PULSE_API_KEY")
	if apiKey == "" {
		log.Fatal("PULSE_API_KEY is not set")
	}
	log.Printf("agent started on %s, sending to %s", host, serverURL)
 
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
 
	for {
		metrics, err := collect(host)
		if err != nil {
			log.Printf("collect failed: %v", err)
		} else if err := send(serverURL, apiKey, metrics); err != nil {
			log.Printf("send failed: %v", err)
		} else {
			log.Printf("sent %d metrics", len(metrics))
		}
		<-ticker.C
	}
}
 
// send converts metrics to JSON and POSTs them to the server.
func send(serverURL, apiKey string, metrics []Metric) error {
	body, err := json.Marshal(metrics)
	if err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}
 
	req, err := http.NewRequest(http.MethodPost, serverURL+"/metrics", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
 
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to server: %w", err)
	}
	defer resp.Body.Close()
 
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("server returned %s", resp.Status)
	}
	return nil
}
 
// collect takes one snapshot of the machine's CPU and memory usage.
func collect(host string) ([]Metric, error) {
	cpuPercents, err := cpu.Percent(time.Second, false)
	if err != nil {
		return nil, fmt.Errorf("reading CPU: %w", err)
	}
	if len(cpuPercents) == 0 {
		return nil, fmt.Errorf("no CPU data returned")
	}
 
	vm, err := mem.VirtualMemory()
	if err != nil {
		return nil, fmt.Errorf("reading memory: %w", err)
	}
 
	now := time.Now().Unix()
	return []Metric{
		{Host: host, Name: "cpu.percent", Value: cpuPercents[0], Timestamp: now},
		{Host: host, Name: "mem.percent", Value: vm.UsedPercent, Timestamp: now},
	}, nil
}
 