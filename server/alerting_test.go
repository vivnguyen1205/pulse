package main

import (
	"math"
	"net/http/httptest"
	"testing"
	"time"

	"pulse/server/anomaly"
)

func TestParseAnomalyConfigDefaultsToCalibratedEWMA(t *testing.T) {
	config, err := parseAnomalyConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if config.detector != "ewma" || config.threshold != 3.8956 ||
		config.baselinePoints != 288 || config.cooldown != 15*time.Minute {
		t.Fatalf("unexpected defaults: %+v", config)
	}
	detector, err := config.newDetector()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := detector.(*anomaly.EWMA); !ok {
		t.Fatalf("default detector is %T, want *anomaly.EWMA", detector)
	}
}

func TestParseAnomalyConfigAcceptsSupportedDetectorsAndOverrides(t *testing.T) {
	tests := []struct {
		name     string
		detector string
		wantType any
	}{
		{"ewma", "EWMA", (*anomaly.EWMA)(nil)},
		{"rolling", "rolling-zscore", (*anomaly.RollingZScore)(nil)},
		{"cusum", "cusum", (*anomaly.CUSUM)(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]string{
				"PULSE_DETECTOR":                 tt.detector,
				"PULSE_DETECTOR_THRESHOLD":       "4.25",
				"PULSE_DETECTOR_BASELINE_POINTS": "12",
				"PULSE_ALERT_COOLDOWN":           "30m",
			}
			config, err := parseAnomalyConfig(func(key string) string { return values[key] })
			if err != nil {
				t.Fatal(err)
			}
			if config.threshold != 4.25 || config.baselinePoints != 12 || config.cooldown != 30*time.Minute {
				t.Fatalf("overrides not applied: %+v", config)
			}
			detector, err := config.newDetector()
			if err != nil {
				t.Fatal(err)
			}
			switch tt.wantType.(type) {
			case *anomaly.EWMA:
				if _, ok := detector.(*anomaly.EWMA); !ok {
					t.Fatalf("got %T, want EWMA", detector)
				}
			case *anomaly.RollingZScore:
				if _, ok := detector.(*anomaly.RollingZScore); !ok {
					t.Fatalf("got %T, want RollingZScore", detector)
				}
			case *anomaly.CUSUM:
				if _, ok := detector.(*anomaly.CUSUM); !ok {
					t.Fatalf("got %T, want CUSUM", detector)
				}
			}
		})
	}
}

func TestDetectorSpecificDefaultThresholds(t *testing.T) {
	tests := []struct {
		detector  string
		threshold float64
	}{
		{"ewma", 3.8956},
		{"rolling-zscore", 3},
		{"cusum", 5},
	}
	for _, tt := range tests {
		config, err := parseAnomalyConfig(func(key string) string {
			if key == "PULSE_DETECTOR" {
				return tt.detector
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		if config.threshold != tt.threshold {
			t.Errorf("%s threshold = %v, want %v", tt.detector, config.threshold, tt.threshold)
		}
	}
}

func TestParseAnomalyConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
	}{
		{"unknown detector", map[string]string{"PULSE_DETECTOR": "other"}},
		{"invalid threshold", map[string]string{"PULSE_DETECTOR_THRESHOLD": "abc"}},
		{"non-finite threshold", map[string]string{"PULSE_DETECTOR_THRESHOLD": "NaN"}},
		{"zero threshold", map[string]string{"PULSE_DETECTOR_THRESHOLD": "0"}},
		{"invalid baseline", map[string]string{"PULSE_DETECTOR_BASELINE_POINTS": "1"}},
		{"invalid cooldown", map[string]string{"PULSE_ALERT_COOLDOWN": "-1m"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseAnomalyConfig(func(key string) string { return tt.values[key] })
			if err == nil {
				t.Fatal("expected invalid configuration to be rejected")
			}
		})
	}
}

func TestAdvanceEpisodeCreatesAndUpdatesPeak(t *testing.T) {
	state := &seriesState{}
	firstTime := time.Unix(100, 0)
	change := advanceEpisode(state, firstTime, anomaly.Observation{Alert: true, Score: 1.2},
		42, 15*time.Minute)
	if !change.create || state.active == nil || state.active.startedAt != firstTime {
		t.Fatalf("first alert should create an episode: %+v / %+v", change, state.active)
	}
	state.active.id = 7

	change = advanceEpisode(state, firstTime.Add(5*time.Minute),
		anomaly.Observation{Alert: true, Score: 1.8}, 55, 15*time.Minute)
	if !change.update || change.create || state.active.peakScore != 1.8 ||
		state.active.peakValue != 55 || state.active.lastAlertAt != firstTime.Add(5*time.Minute) {
		t.Fatalf("higher score should update the episode peak: %+v / %+v", change, state.active)
	}

	change = advanceEpisode(state, firstTime.Add(10*time.Minute),
		anomaly.Observation{Alert: true, Score: 1.1}, 33, 15*time.Minute)
	if !change.update || state.active.peakScore != 1.8 || state.active.peakValue != 55 {
		t.Fatalf("lower score should preserve peak value and score: %+v / %+v", change, state.active)
	}
}

func TestAdvanceEpisodeCooldownClosesAndStartsANewEpisode(t *testing.T) {
	firstTime := time.Unix(100, 0)
	state := &seriesState{active: &activeEpisode{
		id: 9, startedAt: firstTime, lastAlertAt: firstTime,
		peakValue: 50, peakScore: 1.1,
	}}

	change := advanceEpisode(state, firstTime.Add(15*time.Minute),
		anomaly.Observation{}, 50, 15*time.Minute)
	if change.closeID != 0 || state.active == nil {
		t.Fatalf("episode should remain open at the cooldown boundary: %+v / %+v", change, state.active)
	}

	nextAlert := firstTime.Add(15*time.Minute + time.Second)
	change = advanceEpisode(state, nextAlert, anomaly.Observation{Alert: true, Score: 1.5},
		60, 15*time.Minute)
	if change.closeID != 9 || change.closeAt != firstTime || !change.create ||
		state.active == nil || state.active.id != 0 || state.active.startedAt != nextAlert {
		t.Fatalf("alert after cooldown should close and start episodes: %+v / %+v", change, state.active)
	}
}

func TestAdvanceEpisodeExpiresOnNormalPoint(t *testing.T) {
	lastAlert := time.Unix(500, 0)
	state := &seriesState{active: &activeEpisode{
		id: 4, startedAt: lastAlert, lastAlertAt: lastAlert,
	}}
	change := advanceEpisode(state, lastAlert.Add(16*time.Minute), anomaly.Observation{},
		0, 15*time.Minute)
	if change.closeID != 4 || change.closeAt != lastAlert || state.active != nil {
		t.Fatalf("normal point after cooldown should close the episode: %+v / %+v", change, state.active)
	}
}

func TestValidateMetrics(t *testing.T) {
	tests := []struct {
		name    string
		metric  Metric
		wantErr bool
	}{
		{"valid", Metric{Host: "host", Name: "cpu", Value: 12.5}, false},
		{"missing host", Metric{Name: "cpu"}, true},
		{"missing metric", Metric{Host: "host"}, true},
		{"nan", Metric{Host: "host", Name: "cpu", Value: math.NaN()}, true},
		{"positive infinity", Metric{Host: "host", Name: "cpu", Value: math.Inf(1)}, true},
		{"negative infinity", Metric{Host: "host", Name: "cpu", Value: math.Inf(-1)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMetrics([]Metric{tt.metric})
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateMetrics error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHandleAlertsRejectsInvalidQueryBeforeDatabaseAccess(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{"bad duration", "?since=soon"},
		{"zero duration", "?since=0s"},
		{"bad limit", "?limit=abc"},
		{"too large limit", "?limit=501"},
		{"zero limit", "?limit=0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &Server{allowedOrigin: "https://dashboard.example"}
			request := httptest.NewRequest("GET", "/alerts"+tt.query, nil)
			response := httptest.NewRecorder()
			server.handleAlerts(response, request)
			if response.Code != 400 {
				t.Fatalf("status = %d, want 400; body: %s", response.Code, response.Body)
			}
			if got := response.Header().Get("Access-Control-Allow-Origin"); got != server.allowedOrigin {
				t.Fatalf("CORS origin = %q, want %q", got, server.allowedOrigin)
			}
		})
	}
}
