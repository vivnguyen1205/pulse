package main

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"pulse/server/anomaly"
)

const (
	defaultDetector       = "ewma"
	defaultAlertThreshold = 3.8956
	defaultBaselinePoints = 288
	defaultAlertCooldown  = 15 * time.Minute
	rollupInterval        = 5 * time.Minute
	rollupLatenessGrace   = time.Minute
	rollupWorkerInterval  = 30 * time.Second
)

type anomalyConfig struct {
	detector       string
	threshold      float64
	baselinePoints int
	cooldown       time.Duration
}

func anomalyConfigFromEnv() (anomalyConfig, error) {
	return parseAnomalyConfig(os.Getenv)
}

func parseAnomalyConfig(getenv func(string) string) (anomalyConfig, error) {
	config := anomalyConfig{
		detector:       defaultDetector,
		threshold:      defaultThresholdForDetector(defaultDetector),
		baselinePoints: defaultBaselinePoints,
		cooldown:       defaultAlertCooldown,
	}
	if value := getenv("PULSE_DETECTOR"); value != "" {
		config.detector = strings.ToLower(value)
		config.threshold = defaultThresholdForDetector(config.detector)
	}
	if value := getenv("PULSE_DETECTOR_THRESHOLD"); value != "" {
		threshold, err := strconv.ParseFloat(value, 64)
		if err != nil || math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 {
			return anomalyConfig{}, fmt.Errorf("PULSE_DETECTOR_THRESHOLD must be a finite number greater than 0")
		}
		config.threshold = threshold
	}
	if value := getenv("PULSE_DETECTOR_BASELINE_POINTS"); value != "" {
		points, err := strconv.Atoi(value)
		if err != nil || points < 2 {
			return anomalyConfig{}, fmt.Errorf("PULSE_DETECTOR_BASELINE_POINTS must be an integer of at least 2")
		}
		config.baselinePoints = points
	}
	if value := getenv("PULSE_ALERT_COOLDOWN"); value != "" {
		cooldown, err := time.ParseDuration(value)
		if err != nil || cooldown <= 0 {
			return anomalyConfig{}, fmt.Errorf("PULSE_ALERT_COOLDOWN must be a positive duration such as 15m")
		}
		config.cooldown = cooldown
	}
	if _, err := config.newDetector(); err != nil {
		return anomalyConfig{}, err
	}
	return config, nil
}

func defaultThresholdForDetector(detector string) float64 {
	switch detector {
	case "rolling-zscore":
		return 3
	case "cusum":
		return 5
	default:
		// 3.8956 is the EWMA threshold calibrated by server/anomaly/backtest
		// against seed 8820261107: 200 fault-free series, target 2 alerting
		// points per 1,000 normal points. The back-test uses alpha=0.2.
		return defaultAlertThreshold
	}
}

func (c anomalyConfig) newDetector() (anomaly.Detector, error) {
	switch c.detector {
	case "ewma":
		return anomaly.NewEWMA(c.baselinePoints, 0.2, c.threshold)
	case "rolling-zscore":
		return anomaly.NewRollingZScore(c.baselinePoints, c.threshold)
	case "cusum":
		return anomaly.NewCUSUM(c.baselinePoints, 0.5, c.threshold)
	default:
		return nil, fmt.Errorf("unsupported PULSE_DETECTOR %q (use ewma, rolling-zscore, or cusum)", c.detector)
	}
}

type seriesKey struct {
	host string
	name string
}

type activeEpisode struct {
	id          int64
	startedAt   time.Time
	lastAlertAt time.Time
	peakValue   float64
	peakScore   float64
}

type seriesState struct {
	detector   anomaly.Detector
	lastBucket time.Time
	active     *activeEpisode
}

type episodeChange struct {
	closeID int64
	closeAt time.Time
	create  bool
	update  bool
}

func advanceEpisode(state *seriesState, timestamp time.Time, observation anomaly.Observation,
	value float64, cooldown time.Duration) episodeChange {
	var change episodeChange
	if state.active != nil && timestamp.Sub(state.active.lastAlertAt) > cooldown {
		change.closeID = state.active.id
		change.closeAt = state.active.lastAlertAt
		state.active = nil
	}
	if !observation.Alert {
		return change
	}
	if state.active == nil {
		state.active = &activeEpisode{
			startedAt:   timestamp,
			lastAlertAt: timestamp,
			peakValue:   value,
			peakScore:   observation.Score,
		}
		change.create = true
		return change
	}
	state.active.lastAlertAt = timestamp
	if observation.Score > state.active.peakScore {
		state.active.peakScore = observation.Score
		state.active.peakValue = value
	}
	change.update = true
	return change
}
