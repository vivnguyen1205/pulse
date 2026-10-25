package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
)

// Metric is one measurement: "on this host, at this time, this thing had this value."
type Metric struct {
	Host      string
	Name      string
	Value     float64
	Timestamp int64
}

func main() {
	host, err := os.Hostname()
	if err != nil {
		log.Fatalf("could not get hostname: %v", err)
	}
	fmt.Printf("Agent started on %s\n", host)

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		metrics, err := collect(host)
		if err != nil {
			log.Printf("collect failed: %v", err)
		} else {
			for _, m := range metrics {
				fmt.Printf("%d  %-10s %-12s %6.1f\n", m.Timestamp, m.Host, m.Name, m.Value)
			}
			fmt.Println("---")
		}
		<-ticker.C
	}
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