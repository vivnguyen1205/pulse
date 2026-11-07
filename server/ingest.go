package main

import (
	"fmt"
	"math"
)

func validateMetrics(metrics []Metric) error {
	for _, metric := range metrics {
		if metric.Host == "" || metric.Name == "" {
			return fmt.Errorf("host and name are required")
		}
		if math.IsNaN(metric.Value) || math.IsInf(metric.Value, 0) {
			return fmt.Errorf("metric values must be finite")
		}
	}
	return nil
}
