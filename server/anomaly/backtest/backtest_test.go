package backtest

import (
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"pulse/server/anomaly"
)

type scheduledDetector struct {
	index   int
	alertAt map[int]bool
}

func (d *scheduledDetector) Observe(float64) (anomaly.Observation, error) {
	alert := d.alertAt[d.index]
	d.index++
	return anomaly.Observation{Alert: alert}, nil
}

func (d *scheduledDetector) Reset() {
	d.index = 0
}

func TestRunIsReproducibleAndUsesFiveEvaluationSeeds(t *testing.T) {
	config := testConfig()
	first, err := Run(config)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same config and seeds produced different results")
	}
	if len(first.EvaluationSeeds) != config.Seeds {
		t.Fatalf("got %d evaluation seeds, want %d", len(first.EvaluationSeeds), config.Seeds)
	}
	if len(first.Defaults) != len(detectorFactories)*len(faultTypes) ||
		len(first.Matched) != len(detectorFactories)*len(faultTypes) {
		t.Fatal("report omitted detector/fault combinations")
	}
	if len(first.DefaultControls) != len(detectorFactories) ||
		len(first.MatchedControls) != len(detectorFactories) {
		t.Fatal("report omitted no-fault controls")
	}
	for _, evalSeed := range first.EvaluationSeeds {
		for offset := 0; offset < config.Trials; offset++ {
			evaluationTrialSeed := evalSeed + int64(offset)
			for trial := 0; trial < config.CalibrationTrials; trial++ {
				calibrationTrialSeed := config.CalibrationSeed + int64(trial)
				if evaluationTrialSeed == calibrationTrialSeed {
					t.Fatal("calibration and evaluation reused a random seed")
				}
			}
		}
	}
	for _, result := range first.Defaults {
		for _, control := range first.DefaultControls {
			if result.Detector == control.Detector &&
				result.FalseAlarmsPer1000 != control.FalseAlarmsPer1000 {
				t.Fatalf("%s fault results must use the matching no-fault control rate",
					result.Detector)
			}
		}
	}
}

func TestDetectionWindows(t *testing.T) {
	config := DefaultConfig()
	if got := DetectionWindow(StepShift, config); got != 12 {
		t.Errorf("step window = %d, want one hour / 12 points", got)
	}
	if got := DetectionWindow(GradualDrift, config); got != 144 {
		t.Errorf("drift window = %d, want half day / 144 points", got)
	}
	if got := DetectionWindow(SingleSpike, config); got != 1 {
		t.Errorf("spike window = %d, want 1 point", got)
	}
}

func TestEvaluateSeriesCountsOnlyTimelyAlertsAsDetection(t *testing.T) {
	config := testConfig()
	config.NormalPoints = 6
	config.FaultPoints = 20
	config.BaselineSize = 2
	config.DailyPeriod = 6

	factory := detectorFactory{
		name: "scheduled",
		new: func(int, float64) (anomaly.Detector, error) {
			return &scheduledDetector{alertAt: map[int]bool{18: true}}, nil
		},
	}
	outcome, err := evaluateSeries(config, factory, 1, StepShift, 1)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.detected {
		t.Fatal("alert after the one-hour step window must not count as a detection")
	}
	if outcome.falseAlarmPoints != 0 {
		t.Fatalf("late alert during an ongoing fault is not a normal-point false alarm")
	}
}

func TestEvaluateSeriesSpikeDetectionUsesInjectedPoint(t *testing.T) {
	config := testConfig()
	config.NormalPoints = 6
	config.FaultPoints = 6
	config.BaselineSize = 2
	config.DailyPeriod = 6

	factory := detectorFactory{
		name: "scheduled",
		new: func(int, float64) (anomaly.Detector, error) {
			return &scheduledDetector{alertAt: map[int]bool{
				2: true, // the injected spike is the first scored alert
				4: true, // later alerts do not change the first-alert delay
			}}, nil
		},
	}
	outcome, err := evaluateSeries(config, factory, 1, SingleSpike, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.detected || outcome.delay != 0 {
		t.Fatalf("spike at onset should be detected with zero delay: %+v", outcome)
	}
	if outcome.falseAlarmPoints != 0 || outcome.falseAlarmEpisodes != 0 {
		t.Fatal("fault-trial false alarms are measured separately on no-fault controls")
	}
}

func TestSpikeAlertAfterInjectedPointIsNotDetection(t *testing.T) {
	config := testConfig()
	config.BaselineSize = 2
	config.FaultPoints = 6

	factory := detectorFactory{
		name: "scheduled",
		new: func(int, float64) (anomaly.Detector, error) {
			return &scheduledDetector{alertAt: map[int]bool{3: true}}, nil
		},
	}
	outcome, err := evaluateSeries(config, factory, 1, SingleSpike, 1)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.detected {
		t.Fatal("first alert after the injected spike must not count as spike detection")
	}
}

func TestEvaluateNoFaultControlTracksAnyAlert(t *testing.T) {
	config := testConfig()
	config.NormalPoints = 6
	config.FaultPoints = 6
	config.BaselineSize = 2
	config.DailyPeriod = 6

	factory := detectorFactory{
		name: "scheduled",
		new: func(int, float64) (anomaly.Detector, error) {
			return &scheduledDetector{alertAt: map[int]bool{2: true, 3: true, 5: true}}, nil
		},
	}
	outcome, err := evaluateSeries(config, factory, 1, NoFault, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.anyAlert {
		t.Fatal("control with alerts must report at least one alert")
	}
	if outcome.falseAlarmPoints != 3 || outcome.falseAlarmEpisodes != 2 {
		t.Fatalf("got %d false points and %d episodes, want 3 and 2",
			outcome.falseAlarmPoints, outcome.falseAlarmEpisodes)
	}
}

func TestControlMetricsNormalizePointAndEpisodeRates(t *testing.T) {
	got := controlForSeed("test", &totals{
		trials:             4,
		anyAlertTrials:     2,
		normalPoints:       600,
		falseAlarmPoints:   3,
		falseAlarmEpisodes: 2,
	}, Config{DailyPeriod: 300})

	if got.AnyAlertRate.Mean != 0.5 {
		t.Errorf("any-alert rate = %v, want 0.5", got.AnyAlertRate.Mean)
	}
	if got.FalseAlarmsPer1000.Mean != 5 {
		t.Errorf("false point rate = %v, want 5 per 1,000", got.FalseAlarmsPer1000.Mean)
	}
	if got.FalseEpisodesPerDay.Mean != 1 {
		t.Errorf("episode rate = %v, want 1 per day", got.FalseEpisodesPerDay.Mean)
	}
}

func TestGenerateSeriesIsSeededAndFaultLabelsAreCorrect(t *testing.T) {
	config := testConfig()
	config.NormalPoints = 10
	config.FaultPoints = 4
	config.DailyPeriod = 6
	config.BaselineSize = 2

	first := generateSeries(config, GradualDrift, rand.New(rand.NewSource(42)))
	second := generateSeries(config, GradualDrift, rand.New(rand.NewSource(42)))
	if !reflect.DeepEqual(first, second) {
		t.Fatal("same seed produced different generated series")
	}

	normal := generateSeries(config, NoFault, rand.New(rand.NewSource(42)))
	step := generateSeries(config, StepShift, rand.New(rand.NewSource(42)))
	spike := generateSeries(config, SingleSpike, rand.New(rand.NewSource(42)))
	for offset := 0; offset < config.FaultPoints; offset++ {
		index := config.BaselineSize + offset
		if got, want := step[index]-normal[index], config.StepShift; math.Abs(got-want) > 1e-10 {
			t.Errorf("step at offset %d is %v, want %v", offset, got, want)
		}
		if got, want := first[index]-normal[index],
			config.DriftTotal*float64(offset+1)/float64(config.FaultPoints); math.Abs(got-want) > 1e-10 {
			t.Errorf("drift at offset %d is %v, want %v", offset, got, want)
		}
		wantSpike := 0.0
		if offset == 0 {
			wantSpike = config.SpikeHeight
		}
		if got := spike[index] - normal[index]; math.Abs(got-wantSpike) > 1e-10 {
			t.Errorf("spike difference at offset %d is %v, want %v", offset, got, wantSpike)
		}
	}
}

func TestSpreadShowsMeanAndSeedRange(t *testing.T) {
	got := spread([]float64{0.8, 0.9, 1.0, 0.7, 0.6})
	if !got.Available || math.Abs(got.Mean-0.8) > 1e-10 || got.Min != 0.6 || got.Max != 1 {
		t.Fatalf("unexpected spread: %+v", got)
	}
	if got := spread(nil); got.Available {
		t.Fatal("empty spread must be marked unavailable")
	}
}

func TestCombineSeedsUsesPooledMedianAndPerSeedRange(t *testing.T) {
	first := Result{
		Detector:          "EWMA",
		Fault:             StepShift,
		DetectionRate:     valueSpread(1),
		MedianDelayPoints: delayForSeed([]int{1, 9}),
		delaySamples:      []int{1, 9},
	}
	second := Result{
		Detector:          "EWMA",
		Fault:             StepShift,
		DetectionRate:     valueSpread(1),
		MedianDelayPoints: delayForSeed([]int{100}),
		delaySamples:      []int{100},
	}
	combined, _ := combineSeeds([]seedResults{
		{faults: []Result{first}},
		{faults: []Result{second}},
	})
	delay := combined[0].MedianDelayPoints
	if !delay.Available || delay.Median != 9 || delay.SeedMin != 5 || delay.SeedMax != 100 {
		t.Fatalf("unexpected pooled median/range: %+v", delay)
	}
}

func TestMarkdownReportsSpreadAndControlChance(t *testing.T) {
	faultMarkdown := MarkdownResults("Matched false-alarm rate", []Result{{
		Detector:            "EWMA",
		Fault:               GradualDrift,
		DetectionRate:       Spread{Mean: 0.9, Min: 0.8, Max: 1, Available: true},
		FalseAlarmsPer1000:  Spread{Mean: 2, Min: 1, Max: 3, Available: true},
		FalseEpisodesPerDay: Spread{Mean: 1, Min: 0.5, Max: 1.5, Available: true},
		MedianDelayPoints:   DelaySummary{Median: 42, SeedMin: 30, SeedMax: 54, Available: true},
	}})
	if !strings.Contains(faultMarkdown, "90.0% [80.0%–100.0%]") ||
		!strings.Contains(faultMarkdown, "42.0 [30.0–54.0]") {
		t.Fatalf("fault table missing mean/range values:\n%s", faultMarkdown)
	}

	controlMarkdown := MarkdownControls("Default settings", []ControlResult{{
		Detector:            "CUSUM",
		AnyAlertRate:        Spread{Mean: 0.2, Min: 0.1, Max: 0.3, Available: true},
		FalseAlarmsPer1000:  Spread{Mean: 2, Min: 1, Max: 3, Available: true},
		FalseEpisodesPerDay: Spread{Mean: 1, Min: 0.5, Max: 1.5, Available: true},
	}})
	if !strings.Contains(controlMarkdown, "20.0% [10.0%–30.0%]") {
		t.Fatalf("control table missing chance of any alert:\n%s", controlMarkdown)
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	config := testConfig()
	config.Trials = 0
	if _, err := Run(config); err == nil {
		t.Fatal("expected zero trials to be rejected")
	}
}

func testConfig() Config {
	config := DefaultConfig()
	config.Trials = 4
	config.Seeds = 5
	config.CalibrationTrials = 8
	config.NormalPoints = 24
	config.FaultPoints = 16
	config.DailyPeriod = 16
	config.BaselineSize = 8
	return config
}
