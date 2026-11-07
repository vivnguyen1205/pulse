package main

import (
	"testing"
	"time"

	"pulse/server/anomaly"
)

func TestDemoDriftExceedsDefaultEWMAThreshold(t *testing.T) {
	detector, err := anomaly.NewEWMA(demoBaselinePoints, 0.2, defaultAlertThreshold)
	if err != nil {
		t.Fatal(err)
	}

	metric := demoSeriesForPrefix("test")[0]
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for point := 0; point < demoBaselinePoints; point++ {
		bucket := start.Add(time.Duration(point) * rollupInterval)
		observeDemoBucket(t, detector, metric, bucket, 0)
	}

	driftStart := start.Add(time.Duration(demoBaselinePoints) * rollupInterval)
	for point := 0; point < demoDriftPoints; point++ {
		bucket := driftStart.Add(time.Duration(point) * rollupInterval)
		drift := demoDriftIncrease * float64(point+1) / demoDriftPoints
		observation := observeDemoBucket(t, detector, metric, bucket, drift)
		if observation.Alert {
			return
		}
	}
	t.Fatal("EWMA did not alert during the seeded gradual drift")
}

func observeDemoBucket(t *testing.T, detector anomaly.Detector, metric demoSeries,
	bucket time.Time, drift float64) anomaly.Observation {
	t.Helper()
	rows := appendDemoSamples(nil, metric, bucket, drift)
	var total float64
	for _, row := range rows {
		value, ok := row[2].(float64)
		if !ok {
			t.Fatalf("generated sample has type %T, want float64", row[2])
		}
		total += value
	}
	observation, err := detector.Observe(total / float64(len(rows)))
	if err != nil {
		t.Fatal(err)
	}
	return observation
}
