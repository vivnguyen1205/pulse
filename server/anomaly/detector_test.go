package anomaly

import (
	"errors"
	"math"
	"testing"
)

func TestRollingZScoreWarmupAndSpike(t *testing.T) {
	detector, err := NewRollingZScore(4, 3)
	if err != nil {
		t.Fatal(err)
	}

	for _, value := range []float64{8, 10, 12, 10} {
		result, err := detector.Observe(value)
		if err != nil {
			t.Fatal(err)
		}
		if result.Alert {
			t.Fatalf("warm-up value %v unexpectedly alerted", value)
		}
	}

	result, err := detector.Observe(20)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Alert {
		t.Fatalf("expected 20 to be an outlier, got score %v", result.Score)
	}
	if result.Score <= 3 {
		t.Fatalf("expected score above threshold, got %v", result.Score)
	}
}

func TestEWMAStaysQuietThenDetectsGradualDrift(t *testing.T) {
	detector, err := NewEWMA(6, 0.4, 2.7)
	if err != nil {
		t.Fatal(err)
	}

	for _, value := range []float64{9, 10, 11, 9, 10, 11, 10, 10, 10} {
		result, err := detector.Observe(value)
		if err != nil {
			t.Fatal(err)
		}
		if result.Alert {
			t.Fatalf("in-control value %v unexpectedly alerted with score %v", value, result.Score)
		}
	}

	alerted := false
	for _, value := range []float64{10.5, 11, 11.5, 12, 12.5} {
		result, err := detector.Observe(value)
		if err != nil {
			t.Fatal(err)
		}
		if result.Alert {
			alerted = true
			break
		}
	}
	if !alerted {
		t.Fatal("expected EWMA to detect the gradual upward drift")
	}
}

func TestCUSUMDetectsSustainedShiftInBothDirections(t *testing.T) {
	for _, shift := range []float64{1, -1} {
		detector, err := NewCUSUM(6, 0.5, 2.5)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []float64{9, 10, 11, 9, 10, 11} {
			if _, err := detector.Observe(value); err != nil {
				t.Fatal(err)
			}
		}

		alerted := false
		for i := 0; i < 6; i++ {
			result, err := detector.Observe(10 + shift)
			if err != nil {
				t.Fatal(err)
			}
			if result.Alert {
				alerted = true
				break
			}
		}
		if !alerted {
			t.Fatalf("expected CUSUM to detect sustained shift %v", shift)
		}
	}
}

func TestDetectorsRejectNonFiniteValuesWithoutChangingState(t *testing.T) {
	newDetectors := []func() (Detector, error){
		func() (Detector, error) { return NewRollingZScore(3, 3) },
		func() (Detector, error) { return NewEWMA(3, 0.3, 3) },
		func() (Detector, error) { return NewCUSUM(3, 0.5, 4) },
	}

	for _, newDetector := range newDetectors {
		detector, err := newDetector()
		if err != nil {
			t.Fatal(err)
		}
		reference, err := newDetector()
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []float64{1, 2, math.NaN(), math.Inf(1), math.Inf(-1), 3, 4} {
			result, err := detector.Observe(value)
			if !isFinite(value) {
				if !errors.Is(err, ErrNonFiniteValue) {
					t.Fatalf("expected ErrNonFiniteValue for %v, got %v", value, err)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			expected, err := reference.Observe(value)
			if err != nil {
				t.Fatal(err)
			}
			if result != expected {
				t.Fatalf("invalid input changed detector state: got %+v, want %+v", result, expected)
			}
		}
	}
}

func TestResetStartsWarmupAgain(t *testing.T) {
	newDetectors := []func() (Detector, error){
		func() (Detector, error) { return NewRollingZScore(2, 2) },
		func() (Detector, error) { return NewEWMA(2, 0.3, 3) },
		func() (Detector, error) { return NewCUSUM(2, 0.5, 4) },
	}

	for _, newDetector := range newDetectors {
		detector, err := newDetector()
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []float64{1, 2, 100} {
			if _, err := detector.Observe(value); err != nil {
				t.Fatal(err)
			}
		}
		detector.Reset()

		result, err := detector.Observe(100)
		if err != nil {
			t.Fatal(err)
		}
		if result.Alert || result.Score != 0 {
			t.Fatalf("expected reset detector to be warming up, got %+v", result)
		}
	}
}

func TestConstructorsRejectInvalidParameters(t *testing.T) {
	tests := []struct {
		name string
		new  func() error
	}{
		{"rolling window", func() error { _, err := NewRollingZScore(1, 3); return err }},
		{"rolling threshold", func() error { _, err := NewRollingZScore(3, 0); return err }},
		{"ewma baseline", func() error { _, err := NewEWMA(1, 0.3, 3); return err }},
		{"ewma alpha", func() error { _, err := NewEWMA(3, 0, 3); return err }},
		{"ewma limit", func() error { _, err := NewEWMA(3, 0.3, math.Inf(1)); return err }},
		{"cusum baseline", func() error { _, err := NewCUSUM(1, 0.5, 4); return err }},
		{"cusum allowance", func() error { _, err := NewCUSUM(3, -0.5, 4); return err }},
		{"cusum interval", func() error { _, err := NewCUSUM(3, 0.5, 0); return err }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.new(); err == nil {
				t.Fatal("expected invalid parameters to be rejected")
			}
		})
	}
}
