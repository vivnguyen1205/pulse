// Package anomaly provides stateful, streaming detectors for one metric series.
//
// Each detector is configured for one host/metric pair. Call Observe once per
// point in timestamp order. Points used to establish a baseline are warm-up
// points and are not reported as anomalies. Reset discards the baseline and
// accumulated state.
//
// RollingZScore compares a point with the recent window's mean. It is simple
// and reacts quickly to isolated outliers and large shifts, but it can miss
// slow drift and a sustained shift can eventually become part of its baseline.
// Its window size controls how much recent history is considered; its
// threshold is the number of standard deviations required to flag a point.
//
// EWMA smooths each new value together with its previous smoothed estimate,
// which lets small, repeated changes accumulate while damping random noise.
// Its baseline size controls how much initial history is used to estimate the
// normal mean and spread. Alpha (0 < alpha <= 1) controls how much the latest
// value influences the estimate: higher values react faster but are noisier.
// The limit multiplier controls the control-chart boundary: higher values
// reduce false alarms but make detection slower or less sensitive.
//
// CUSUM accumulates evidence that values are consistently above or below the
// baseline mean, making it useful for small sustained shifts. Its baseline
// size estimates the normal mean and spread. The allowance is the shift, in
// standard deviations, ignored on each point; a larger allowance is less
// sensitive to small shifts. The decision interval is the accumulated
// evidence required to flag an anomaly; a larger interval reduces false
// alarms but increases detection delay. After a signal, its accumulated
// evidence resets so a continuing issue can produce a new signal later.
//
// Scores are detector-specific: RollingZScore reports absolute z-score,
// EWMA reports distance from the baseline mean as a fraction of its current
// control limit, and CUSUM reports its largest accumulated evidence value.
// Baseline and rolling spread estimates use population standard deviation.
package anomaly

import (
	"errors"
	"fmt"
	"math"
)

// ErrNonFiniteValue is returned when Observe receives NaN or an infinity.
var ErrNonFiniteValue = errors.New("metric value must be finite")

// Observation is the result of evaluating one point. Score meaning depends on
// the detector; each detector's score is documented in the package comment.
type Observation struct {
	Alert bool
	Score float64
}

// Detector is a stateful detector for a single ordered metric series.
type Detector interface {
	Observe(value float64) (Observation, error)
	Reset()
}

// RollingZScore flags values far from the mean of the preceding window.
type RollingZScore struct {
	windowSize int
	threshold  float64
	history    []float64
}

// NewRollingZScore creates a rolling z-score detector. The window must contain
// at least two points; the threshold must be finite and greater than zero.
func NewRollingZScore(windowSize int, threshold float64) (*RollingZScore, error) {
	if windowSize < 2 {
		return nil, fmt.Errorf("window size must be at least 2")
	}
	if !isFinite(threshold) || threshold <= 0 {
		return nil, fmt.Errorf("threshold must be finite and greater than 0")
	}
	return &RollingZScore{windowSize: windowSize, threshold: threshold}, nil
}

// Observe scores a point against the preceding window, then makes it part of
// the window. The first windowSize points are baseline warm-up.
func (d *RollingZScore) Observe(value float64) (Observation, error) {
	if err := validateValue(value); err != nil {
		return Observation{}, err
	}

	result := Observation{}
	if len(d.history) == d.windowSize {
		mean, stddev := meanStdDev(d.history)
		result.Score = standardizedDistance(value, mean, stddev)
		result.Alert = result.Score > d.threshold
	}

	if len(d.history) < d.windowSize {
		d.history = append(d.history, value)
	} else {
		copy(d.history, d.history[1:])
		d.history[len(d.history)-1] = value
	}
	return result, nil
}

// Reset clears the rolling baseline.
func (d *RollingZScore) Reset() {
	d.history = nil
}

// EWMA is an exponentially weighted moving average control-chart detector.
type EWMA struct {
	baselineSize int
	alpha        float64
	limit        float64
	baseline     []float64
	mean         float64
	stddev       float64
	estimate     float64
	steps        int
	initialized  bool
}

// NewEWMA creates an EWMA detector. baselineSize must be at least two, alpha
// must be in (0, 1], and limit must be finite and greater than zero.
func NewEWMA(baselineSize int, alpha, limit float64) (*EWMA, error) {
	if baselineSize < 2 {
		return nil, fmt.Errorf("baseline size must be at least 2")
	}
	if !isFinite(alpha) || alpha <= 0 || alpha > 1 {
		return nil, fmt.Errorf("alpha must be finite and in (0, 1]")
	}
	if !isFinite(limit) || limit <= 0 {
		return nil, fmt.Errorf("limit must be finite and greater than 0")
	}
	return &EWMA{
		baselineSize: baselineSize,
		alpha:        alpha,
		limit:        limit,
	}, nil
}

// Observe updates the smoothed estimate and tests it against the time-varying
// EWMA control limit. The first baselineSize points establish the baseline.
func (d *EWMA) Observe(value float64) (Observation, error) {
	if err := validateValue(value); err != nil {
		return Observation{}, err
	}

	if !d.initialized {
		d.baseline = append(d.baseline, value)
		if len(d.baseline) == d.baselineSize {
			d.mean, d.stddev = meanStdDev(d.baseline)
			d.estimate = d.mean
			d.initialized = true
		}
		return Observation{}, nil
	}

	d.estimate = d.alpha*value + (1-d.alpha)*d.estimate
	d.steps++
	factor := d.alpha / (2 - d.alpha) *
		(1 - math.Pow(1-d.alpha, float64(2*d.steps)))
	controlLimit := d.limit * d.stddev * math.Sqrt(factor)
	score := distanceRatio(d.estimate, d.mean, controlLimit)
	return Observation{Alert: score > 1, Score: score}, nil
}

// Reset discards the baseline and the smoothed estimate.
func (d *EWMA) Reset() {
	d.baseline = nil
	d.mean = 0
	d.stddev = 0
	d.estimate = 0
	d.steps = 0
	d.initialized = false
}

// CUSUM is a two-sided cumulative-sum detector.
type CUSUM struct {
	baselineSize int
	allowance    float64
	interval     float64
	baseline     []float64
	mean         float64
	stddev       float64
	upper        float64
	lower        float64
	initialized  bool
}

// NewCUSUM creates a two-sided CUSUM detector. baselineSize must be at least
// two, allowance must be finite and nonnegative, and interval must be finite
// and greater than zero. Allowance and interval are measured in standard
// deviations.
func NewCUSUM(baselineSize int, allowance, interval float64) (*CUSUM, error) {
	if baselineSize < 2 {
		return nil, fmt.Errorf("baseline size must be at least 2")
	}
	if !isFinite(allowance) || allowance < 0 {
		return nil, fmt.Errorf("allowance must be finite and nonnegative")
	}
	if !isFinite(interval) || interval <= 0 {
		return nil, fmt.Errorf("interval must be finite and greater than 0")
	}
	return &CUSUM{
		baselineSize: baselineSize,
		allowance:    allowance,
		interval:     interval,
	}, nil
}

// Observe accumulates standardized evidence above and below the baseline.
// The first baselineSize points establish the baseline. A signal resets both
// cumulative sums before the next point is evaluated.
func (d *CUSUM) Observe(value float64) (Observation, error) {
	if err := validateValue(value); err != nil {
		return Observation{}, err
	}

	if !d.initialized {
		d.baseline = append(d.baseline, value)
		if len(d.baseline) == d.baselineSize {
			d.mean, d.stddev = meanStdDev(d.baseline)
			d.initialized = true
		}
		return Observation{}, nil
	}

	z := signedStandardizedDifference(value, d.mean, d.stddev)
	d.upper = nonnegativeSaturating(d.upper + z - d.allowance)
	d.lower = nonnegativeSaturating(d.lower - z - d.allowance)
	score := math.Max(d.upper, d.lower)
	alert := score > d.interval
	if alert {
		d.upper = 0
		d.lower = 0
	}
	return Observation{Alert: alert, Score: score}, nil
}

// Reset discards the baseline and both cumulative sums.
func (d *CUSUM) Reset() {
	d.baseline = nil
	d.mean = 0
	d.stddev = 0
	d.upper = 0
	d.lower = 0
	d.initialized = false
}

func meanStdDev(values []float64) (float64, float64) {
	scale := 0.0
	for _, value := range values {
		scale = math.Max(scale, math.Abs(value))
	}
	if scale == 0 {
		return 0, 0
	}

	normalizedMean := 0.0
	for _, value := range values {
		normalizedMean += (value / scale) / float64(len(values))
	}
	variance := 0.0
	for _, value := range values {
		difference := value/scale - normalizedMean
		variance += difference * difference / float64(len(values))
	}
	return normalizedMean * scale, math.Sqrt(variance) * scale
}

func standardizedDistance(value, mean, stddev float64) float64 {
	difference := value - mean
	if stddev == 0 {
		if difference == 0 {
			return 0
		}
		return math.MaxFloat64
	}
	if math.IsInf(difference, 0) {
		return math.MaxFloat64
	}
	score := math.Abs(difference) / stddev
	if math.IsInf(score, 0) {
		return math.MaxFloat64
	}
	return score
}

func signedStandardizedDifference(value, mean, stddev float64) float64 {
	difference := value - mean
	if stddev == 0 {
		if difference == 0 {
			return 0
		}
		return math.Copysign(math.MaxFloat64, difference)
	}
	if math.IsInf(difference, 0) {
		return math.Copysign(math.MaxFloat64, difference)
	}
	z := difference / stddev
	if math.IsInf(z, 0) {
		return math.Copysign(math.MaxFloat64, z)
	}
	return z
}

func distanceRatio(value, mean, limit float64) float64 {
	difference := value - mean
	if limit == 0 {
		if difference == 0 {
			return 0
		}
		return math.MaxFloat64
	}
	if math.IsInf(difference, 0) {
		return math.MaxFloat64
	}
	score := math.Abs(difference) / limit
	if math.IsInf(score, 0) {
		return math.MaxFloat64
	}
	return score
}

func nonnegativeSaturating(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 1) || value > math.MaxFloat64 {
		return math.MaxFloat64
	}
	if value < 0 {
		return 0
	}
	return value
}

func validateValue(value float64) error {
	if !isFinite(value) {
		return ErrNonFiniteValue
	}
	return nil
}

func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
