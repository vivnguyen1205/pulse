package anomaly

import "fmt"

// Clone returns an independent copy of a detector and its accumulated state.
func Clone(detector Detector) (Detector, error) {
	switch current := detector.(type) {
	case *RollingZScore:
		cloned := *current
		cloned.history = append([]float64(nil), current.history...)
		return &cloned, nil
	case *EWMA:
		cloned := *current
		cloned.baseline = append([]float64(nil), current.baseline...)
		return &cloned, nil
	case *CUSUM:
		cloned := *current
		cloned.baseline = append([]float64(nil), current.baseline...)
		return &cloned, nil
	default:
		return nil, fmt.Errorf("cannot clone detector type %T", detector)
	}
}
