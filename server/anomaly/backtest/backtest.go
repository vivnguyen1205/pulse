// Package backtest generates labeled metric series and measures anomaly
// detectors against them.
package backtest

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"

	"pulse/server/anomaly"
)

type FaultType string

const (
	NoFault      FaultType = "no fault"
	StepShift    FaultType = "step shift"
	GradualDrift FaultType = "gradual drift"
	SingleSpike  FaultType = "single spike"
)

// Config controls the reproducible synthetic experiment.
type Config struct {
	Trials                   int
	Seeds                    int
	Seed                     int64
	CalibrationTrials        int
	CalibrationSeed          int64
	TargetFalseAlarmsPer1000 float64
	NormalPoints             int
	FaultPoints              int
	DailyPeriod              int
	BaselineSize             int
	Mean                     float64
	DailyAmplitude           float64
	NoiseStdDev              float64
	StepShift                float64
	DriftTotal               float64
	SpikeHeight              float64
}

// DefaultConfig describes a one-day detector warm-up followed by a one-day
// fault or fault-free evaluation horizon, sampled every five minutes.
func DefaultConfig() Config {
	return Config{
		Trials:                   200,
		Seeds:                    5,
		Seed:                     20261107,
		CalibrationTrials:        200,
		CalibrationSeed:          8820261107,
		TargetFalseAlarmsPer1000: 2,
		NormalPoints:             288,
		FaultPoints:              288,
		DailyPeriod:              288,
		BaselineSize:             288,
		Mean:                     50,
		DailyAmplitude:           1,
		NoiseStdDev:              2,
		StepShift:                6,
		DriftTotal:               6,
		SpikeHeight:              10,
	}
}

// Spread summarizes one metric across the independent evaluation seeds.
type Spread struct {
	Mean      float64
	Min       float64
	Max       float64
	Available bool
}

// DelaySummary reports the pooled median delay and the range of median delays
// across evaluation seeds.
type DelaySummary struct {
	Median    float64
	SeedMin   float64
	SeedMax   float64
	Available bool
}

// Result summarizes one detector and fault type across evaluation seeds.
type Result struct {
	Detector            string
	Fault               FaultType
	DetectionRate       Spread
	FalseAlarmsPer1000  Spread
	FalseEpisodesPerDay Spread
	MedianDelayPoints   DelaySummary
	delaySamples        []int
}

// ControlResult reports how often an entirely fault-free series still caused
// an alert, along with point and episode false-alarm rates.
type ControlResult struct {
	Detector            string
	AnyAlertRate        Spread
	FalseAlarmsPer1000  Spread
	FalseEpisodesPerDay Spread
}

// CalibrationResult records the selected scalar threshold and the point-level
// false-alarm rate measured on the independent calibration data.
type CalibrationResult struct {
	Detector        string
	Threshold       float64
	TargetPer1000   float64
	AchievedPer1000 float64
}

// Report contains both the original/default parameter comparison and the
// comparison after independent false-alarm calibration.
type Report struct {
	EvaluationSeeds []int64
	Defaults        []Result
	DefaultControls []ControlResult
	Calibration     []CalibrationResult
	Matched         []Result
	MatchedControls []ControlResult
}

type detectorFactory struct {
	name             string
	defaultThreshold float64
	new              func(int, float64) (anomaly.Detector, error)
}

var detectorFactories = []detectorFactory{
	{
		name:             "Rolling z-score",
		defaultThreshold: 3,
		new: func(baselineSize int, threshold float64) (anomaly.Detector, error) {
			return anomaly.NewRollingZScore(baselineSize, threshold)
		},
	},
	{
		name:             "EWMA",
		defaultThreshold: 3.5,
		new: func(baselineSize int, threshold float64) (anomaly.Detector, error) {
			return anomaly.NewEWMA(baselineSize, 0.2, threshold)
		},
	},
	{
		name:             "CUSUM",
		defaultThreshold: 5,
		new: func(baselineSize int, threshold float64) (anomaly.Detector, error) {
			return anomaly.NewCUSUM(baselineSize, 0.5, threshold)
		},
	},
}

var faultTypes = []FaultType{StepShift, GradualDrift, SingleSpike}

type settings map[string]float64

type trialOutcome struct {
	detected           bool
	delay              int
	normalPoints       int
	falseAlarmPoints   int
	falseAlarmEpisodes int
	anyAlert           bool
}

type totals struct {
	trials             int
	detected           int
	delays             []int
	normalPoints       int
	falseAlarmPoints   int
	falseAlarmEpisodes int
	anyAlertTrials     int
}

type seedResults struct {
	faults   []Result
	controls []ControlResult
}

// Run calibrates each detector on independent fault-free data, then compares
// default and calibrated thresholds across independent evaluation seeds.
//
// The first alert after warm-up must be on a labeled fault point and within
// its detection window. Step shifts use one hour (12 points); gradual drift
// uses half a day (DailyPeriod/2 points); a spike uses its injected point.
// Separate no-fault controls measure false alarms over the same horizon.
func Run(config Config) (Report, error) {
	if err := validateConfig(config); err != nil {
		return Report{}, err
	}

	thresholds, calibration, err := calibrate(config)
	if err != nil {
		return Report{}, err
	}

	report := Report{Calibration: calibration}
	defaultBySeed := make([]seedResults, 0, config.Seeds)
	matchedBySeed := make([]seedResults, 0, config.Seeds)
	for seedIndex := 0; seedIndex < config.Seeds; seedIndex++ {
		seed := config.Seed + int64(seedIndex)*1_000_003
		report.EvaluationSeeds = append(report.EvaluationSeeds, seed)
		defaultResults, err := evaluateSeed(config, seed, defaultSettings())
		if err != nil {
			return Report{}, err
		}
		matchedResults, err := evaluateSeed(config, seed, thresholds)
		if err != nil {
			return Report{}, err
		}
		defaultBySeed = append(defaultBySeed, defaultResults)
		matchedBySeed = append(matchedBySeed, matchedResults)
	}

	report.Defaults, report.DefaultControls = combineSeeds(defaultBySeed)
	report.Matched, report.MatchedControls = combineSeeds(matchedBySeed)
	return report, nil
}

// DetectionWindow returns the maximum number of points after onset in which
// an alert can count as timely detection. Step: one hour; drift: half a day;
// spike: the single injected point.
func DetectionWindow(fault FaultType, config Config) int {
	switch fault {
	case StepShift:
		return min(12, config.FaultPoints)
	case GradualDrift:
		return min(max(1, config.DailyPeriod/2), config.FaultPoints)
	case SingleSpike:
		return min(1, config.FaultPoints)
	default:
		return 0
	}
}

// MarkdownResults formats fault-detection measurements with seed-to-seed
// ranges, ready to paste into a README.
func MarkdownResults(title string, results []Result) string {
	var output strings.Builder
	output.WriteString("### " + title + "\n\n")
	output.WriteString("| Detector | Fault | Timely detection rate | False alarm points / 1,000 normal points | False alarm episodes / normal day | Pooled median delay [seed min–max] (points) |\n")
	output.WriteString("|---|---|---:|---:|---:|---:|\n")
	for _, result := range results {
		fmt.Fprintf(&output, "| %s | %s | %s | %s | %s | %s |\n",
			result.Detector, result.Fault, formatSpread(result.DetectionRate, true),
			formatSpread(result.FalseAlarmsPer1000, false),
			formatSpread(result.FalseEpisodesPerDay, false),
			formatDelay(result.MedianDelayPoints))
	}
	return output.String()
}

// MarkdownControls formats the no-fault control measurements.
func MarkdownControls(title string, controls []ControlResult) string {
	var output strings.Builder
	output.WriteString("### " + title + " (no-fault control)\n\n")
	output.WriteString("| Detector | Chance of any alert in control series | False alarm points / 1,000 normal points | False alarm episodes / normal day |\n")
	output.WriteString("|---|---:|---:|---:|\n")
	for _, control := range controls {
		fmt.Fprintf(&output, "| %s | %s | %s | %s |\n",
			control.Detector, formatSpread(control.AnyAlertRate, true),
			formatSpread(control.FalseAlarmsPer1000, false),
			formatSpread(control.FalseEpisodesPerDay, false))
	}
	return output.String()
}

// MarkdownCalibration formats the threshold calibration audit trail.
func MarkdownCalibration(results []CalibrationResult) string {
	var output strings.Builder
	output.WriteString("### Matched-rate calibration\n\n")
	output.WriteString("| Detector | Tuned threshold | Target false alarm points / 1,000 | Calibration rate / 1,000 |\n")
	output.WriteString("|---|---:|---:|---:|\n")
	for _, result := range results {
		fmt.Fprintf(&output, "| %s | %.4f | %.2f | %.2f |\n",
			result.Detector, result.Threshold, result.TargetPer1000, result.AchievedPer1000)
	}
	return output.String()
}

func validateConfig(config Config) error {
	if config.Trials < 1 || config.Seeds < 1 || config.CalibrationTrials < 1 {
		return fmt.Errorf("trials, seeds, and calibration trials must be at least 1")
	}
	if config.NormalPoints < 1 {
		return fmt.Errorf("normal evaluation points must be at least 1")
	}
	if config.FaultPoints < 1 || config.DailyPeriod < 1 || config.BaselineSize < 2 {
		return fmt.Errorf("fault points, daily period, and baseline size must be positive (baseline at least 2)")
	}
	if !finite(config.Mean) || !finite(config.DailyAmplitude) || config.DailyAmplitude < 0 {
		return fmt.Errorf("mean must be finite and daily amplitude must be finite and nonnegative")
	}
	if !finite(config.NoiseStdDev) || config.NoiseStdDev <= 0 {
		return fmt.Errorf("noise standard deviation must be finite and greater than 0")
	}
	if !finite(config.TargetFalseAlarmsPer1000) || config.TargetFalseAlarmsPer1000 <= 0 {
		return fmt.Errorf("target false alarm rate must be finite and greater than 0")
	}
	for name, value := range map[string]float64{
		"step shift": config.StepShift, "drift total": config.DriftTotal,
		"spike height": config.SpikeHeight,
	} {
		if !finite(value) {
			return fmt.Errorf("%s must be finite", name)
		}
	}
	return nil
}

func defaultSettings() settings {
	result := make(settings, len(detectorFactories))
	for _, factory := range detectorFactories {
		result[factory.name] = factory.defaultThreshold
	}
	return result
}

func calibrate(config Config) (settings, []CalibrationResult, error) {
	selected := make(settings, len(detectorFactories))
	results := make([]CalibrationResult, 0, len(detectorFactories))
	for _, factory := range detectorFactories {
		low, high := 0.1, 100.0
		bestThreshold := high
		bestRate, err := calibrationRate(config, factory, high)
		if err != nil {
			return nil, nil, err
		}
		bestError := math.Abs(bestRate - config.TargetFalseAlarmsPer1000)

		for iteration := 0; iteration < 16; iteration++ {
			candidate := (low + high) / 2
			rate, err := calibrationRate(config, factory, candidate)
			if err != nil {
				return nil, nil, err
			}
			difference := math.Abs(rate - config.TargetFalseAlarmsPer1000)
			if difference < bestError {
				bestThreshold, bestRate, bestError = candidate, rate, difference
			}
			if rate > config.TargetFalseAlarmsPer1000 {
				low = candidate
			} else {
				high = candidate
			}
		}
		selected[factory.name] = bestThreshold
		results = append(results, CalibrationResult{
			Detector:        factory.name,
			Threshold:       bestThreshold,
			TargetPer1000:   config.TargetFalseAlarmsPer1000,
			AchievedPer1000: bestRate,
		})
	}
	return selected, results, nil
}

func calibrationRate(config Config, factory detectorFactory, threshold float64) (float64, error) {
	pointsPerSeries := config.NormalPoints
	falsePoints := 0
	for trial := 0; trial < config.CalibrationTrials; trial++ {
		seed := config.CalibrationSeed + int64(trial)
		rng := rand.New(rand.NewSource(seed))
		series := generateSeries(config, NoFault, rng)
		detector, err := factory.new(config.BaselineSize, threshold)
		if err != nil {
			return 0, fmt.Errorf("creating %s detector for calibration: %w", factory.name, err)
		}
		for index, value := range series {
			observation, err := detector.Observe(value)
			if err != nil {
				return 0, fmt.Errorf("%s calibration trial %d point %d: %w",
					factory.name, trial, index, err)
			}
			if index >= config.BaselineSize && observation.Alert {
				falsePoints++
			}
		}
	}
	totalPoints := config.CalibrationTrials * pointsPerSeries
	return 1000 * float64(falsePoints) / float64(totalPoints), nil
}

func evaluateSeed(config Config, seed int64, thresholds settings) (seedResults, error) {
	resultTotals := make(map[string]*totals, len(detectorFactories)*len(faultTypes))
	controlTotals := make(map[string]*totals, len(detectorFactories))
	for _, factory := range detectorFactories {
		controlTotals[factory.name] = &totals{}
		for _, fault := range faultTypes {
			resultTotals[key(factory.name, fault)] = &totals{}
		}
	}

	for trial := 0; trial < config.Trials; trial++ {
		trialSeed := seed + int64(trial)
		for _, factory := range detectorFactories {
			threshold := thresholds[factory.name]
			control, err := evaluateSeries(config, factory, threshold, NoFault, trialSeed)
			if err != nil {
				return seedResults{}, err
			}
			addOutcome(controlTotals[factory.name], control)

			for _, fault := range faultTypes {
				outcome, err := evaluateSeries(config, factory, threshold, fault, trialSeed)
				if err != nil {
					return seedResults{}, err
				}
				addOutcome(resultTotals[key(factory.name, fault)], outcome)
			}
		}
	}

	result := seedResults{}
	for _, factory := range detectorFactories {
		for _, fault := range faultTypes {
			result.faults = append(result.faults, resultForSeed(factory.name, fault,
				resultTotals[key(factory.name, fault)], controlTotals[factory.name], config))
		}
		result.controls = append(result.controls,
			controlForSeed(factory.name, controlTotals[factory.name], config))
	}
	return result, nil
}

func evaluateSeries(config Config, factory detectorFactory, threshold float64, fault FaultType, seed int64) (trialOutcome, error) {
	rng := rand.New(rand.NewSource(seed))
	series := generateSeries(config, fault, rng)
	detector, err := factory.new(config.BaselineSize, threshold)
	if err != nil {
		return trialOutcome{}, fmt.Errorf("creating %s detector: %w", factory.name, err)
	}

	outcome := trialOutcome{delay: -1}
	inNormalEpisode := false
	firstAlertIndex := -1
	window := DetectionWindow(fault, config)
	for index, value := range series {
		observation, err := detector.Observe(value)
		if err != nil {
			return trialOutcome{}, fmt.Errorf("%s on %s at point %d: %w",
				factory.name, fault, index, err)
		}
		if index < config.BaselineSize {
			continue
		}
		if observation.Alert && firstAlertIndex < 0 {
			firstAlertIndex = index
		}

		offset := index - config.BaselineSize
		isFault := fault != NoFault && offset >= 0 && isFaultPoint(fault, offset)
		if isFault {
			inNormalEpisode = false
			if firstAlertIndex == index && offset < window {
				outcome.detected = true
				outcome.delay = offset
			}
			continue
		}

		if fault != NoFault {
			continue
		}
		outcome.normalPoints++
		if observation.Alert {
			outcome.anyAlert = true
			outcome.falseAlarmPoints++
			if !inNormalEpisode {
				outcome.falseAlarmEpisodes++
			}
			inNormalEpisode = true
		} else {
			inNormalEpisode = false
		}
	}
	return outcome, nil
}

func isFaultPoint(fault FaultType, offset int) bool {
	if offset < 0 {
		return false
	}
	switch fault {
	case StepShift, GradualDrift:
		return true
	case SingleSpike:
		return offset == 0
	default:
		return false
	}
}

func addOutcome(sum *totals, outcome trialOutcome) {
	sum.trials++
	sum.normalPoints += outcome.normalPoints
	sum.falseAlarmPoints += outcome.falseAlarmPoints
	sum.falseAlarmEpisodes += outcome.falseAlarmEpisodes
	if outcome.anyAlert {
		sum.anyAlertTrials++
	}
	if outcome.detected {
		sum.detected++
		sum.delays = append(sum.delays, outcome.delay)
	}
}

func resultForSeed(detector string, fault FaultType, sum, control *totals, config Config) Result {
	normalDays := float64(control.normalPoints) / float64(config.DailyPeriod)
	falsePointsPer1000 := ratePer1000(control.falseAlarmPoints, control.normalPoints)
	episodesPerDay := 0.0
	if normalDays > 0 {
		episodesPerDay = float64(control.falseAlarmEpisodes) / normalDays
	}
	return Result{
		Detector:            detector,
		Fault:               fault,
		DetectionRate:       valueSpread(float64(sum.detected) / float64(sum.trials)),
		FalseAlarmsPer1000:  valueSpread(falsePointsPer1000),
		FalseEpisodesPerDay: valueSpread(episodesPerDay),
		MedianDelayPoints:   delayForSeed(sum.delays),
		delaySamples:        append([]int(nil), sum.delays...),
	}
}

func controlForSeed(detector string, sum *totals, config Config) ControlResult {
	normalDays := float64(sum.normalPoints) / float64(config.DailyPeriod)
	episodesPerDay := 0.0
	if normalDays > 0 {
		episodesPerDay = float64(sum.falseAlarmEpisodes) / normalDays
	}
	return ControlResult{
		Detector:            detector,
		AnyAlertRate:        valueSpread(float64(sum.anyAlertTrials) / float64(sum.trials)),
		FalseAlarmsPer1000:  valueSpread(ratePer1000(sum.falseAlarmPoints, sum.normalPoints)),
		FalseEpisodesPerDay: valueSpread(episodesPerDay),
	}
}

func combineSeeds(seeds []seedResults) ([]Result, []ControlResult) {
	results := make([]Result, 0, len(detectorFactories)*len(faultTypes))
	controls := make([]ControlResult, 0, len(detectorFactories))
	for resultIndex := range seeds[0].faults {
		result := Result{
			Detector: seeds[0].faults[resultIndex].Detector,
			Fault:    seeds[0].faults[resultIndex].Fault,
		}
		var detectionRates, falsePoints, falseEpisodes, seedDelays []float64
		var allDelays []int
		for _, seed := range seeds {
			current := seed.faults[resultIndex]
			detectionRates = append(detectionRates, current.DetectionRate.Mean)
			falsePoints = append(falsePoints, current.FalseAlarmsPer1000.Mean)
			falseEpisodes = append(falseEpisodes, current.FalseEpisodesPerDay.Mean)
			if current.MedianDelayPoints.Available {
				seedDelays = append(seedDelays, current.MedianDelayPoints.Median)
				allDelays = append(allDelays, current.delaySamples...)
			}
		}
		result.DetectionRate = spread(detectionRates)
		result.FalseAlarmsPer1000 = spread(falsePoints)
		result.FalseEpisodesPerDay = spread(falseEpisodes)
		result.MedianDelayPoints = combineDelaySeeds(allDelays, seedDelays)
		results = append(results, result)
	}
	for controlIndex := range seeds[0].controls {
		control := ControlResult{Detector: seeds[0].controls[controlIndex].Detector}
		var anyAlert, falsePoints, falseEpisodes []float64
		for _, seed := range seeds {
			current := seed.controls[controlIndex]
			anyAlert = append(anyAlert, current.AnyAlertRate.Mean)
			falsePoints = append(falsePoints, current.FalseAlarmsPer1000.Mean)
			falseEpisodes = append(falseEpisodes, current.FalseEpisodesPerDay.Mean)
		}
		control.AnyAlertRate = spread(anyAlert)
		control.FalseAlarmsPer1000 = spread(falsePoints)
		control.FalseEpisodesPerDay = spread(falseEpisodes)
		controls = append(controls, control)
	}
	return results, controls
}

func generateSeries(config Config, fault FaultType, rng *rand.Rand) []float64 {
	pointsAfterBaseline := config.FaultPoints
	if fault == NoFault {
		pointsAfterBaseline = config.NormalPoints
	}
	series := make([]float64, config.BaselineSize+pointsAfterBaseline)
	phase := rng.Float64() * 2 * math.Pi
	for index := range series {
		angle := 2*math.Pi*float64(index)/float64(config.DailyPeriod) + phase
		series[index] = config.Mean +
			config.DailyAmplitude*math.Sin(angle) +
			rng.NormFloat64()*config.NoiseStdDev
	}

	if fault == NoFault {
		return series
	}
	for offset := 0; offset < config.FaultPoints; offset++ {
		index := config.BaselineSize + offset
		switch fault {
		case StepShift:
			series[index] += config.StepShift
		case GradualDrift:
			series[index] += config.DriftTotal * float64(offset+1) / float64(config.FaultPoints)
		case SingleSpike:
			if offset == 0 {
				series[index] += config.SpikeHeight
			}
		}
	}
	return series
}

func ratePer1000(alerts, points int) float64 {
	if points == 0 {
		return 0
	}
	return 1000 * float64(alerts) / float64(points)
}

func median(values []int) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int(nil), values...)
	sort.Ints(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return float64(sorted[middle])
	}
	return float64(sorted[middle-1]+sorted[middle]) / 2
}

func valueSpread(value float64) Spread {
	return Spread{Mean: value, Min: value, Max: value, Available: true}
}

func delayForSeed(values []int) DelaySummary {
	if len(values) == 0 {
		return DelaySummary{}
	}
	value := median(values)
	return DelaySummary{Median: value, SeedMin: value, SeedMax: value, Available: true}
}

func combineDelaySeeds(allDelays []int, seedMedians []float64) DelaySummary {
	if len(allDelays) == 0 || len(seedMedians) == 0 {
		return DelaySummary{}
	}
	seedSpread := spread(seedMedians)
	return DelaySummary{
		Median:    median(allDelays),
		SeedMin:   seedSpread.Min,
		SeedMax:   seedSpread.Max,
		Available: true,
	}
}

func spread(values []float64) Spread {
	if len(values) == 0 {
		return Spread{}
	}
	sum := 0.0
	minValue, maxValue := values[0], values[0]
	for _, value := range values {
		sum += value
		minValue = math.Min(minValue, value)
		maxValue = math.Max(maxValue, value)
	}
	return Spread{
		Mean:      sum / float64(len(values)),
		Min:       minValue,
		Max:       maxValue,
		Available: true,
	}
}

func formatSpread(value Spread, percentage bool) string {
	if !value.Available {
		return "—"
	}
	suffix := ""
	scale := 1.0
	if percentage {
		suffix = "%"
		scale = 100
	}
	return fmt.Sprintf("%.1f%s [%.1f%s–%.1f%s]",
		value.Mean*scale, suffix, value.Min*scale, suffix, value.Max*scale, suffix)
}

func formatDelay(value DelaySummary) string {
	if !value.Available {
		return "—"
	}
	return fmt.Sprintf("%.1f [%.1f–%.1f]", value.Median, value.SeedMin, value.SeedMax)
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func key(detector string, fault FaultType) string {
	return detector + "\x00" + string(fault)
}
