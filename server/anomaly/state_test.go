package anomaly

import "testing"

func TestClonePreservesStateAndBranchesIndependently(t *testing.T) {
	factories := []struct {
		name string
		new  func() (Detector, error)
	}{
		{"rolling", func() (Detector, error) { return NewRollingZScore(2, 2) }},
		{"ewma", func() (Detector, error) { return NewEWMA(2, 0.3, 3) }},
		{"cusum", func() (Detector, error) { return NewCUSUM(2, 0.5, 4) }},
	}
	for _, factory := range factories {
		t.Run(factory.name, func(t *testing.T) {
			original, err := factory.new()
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []float64{1, 2} {
				if _, err := original.Observe(value); err != nil {
					t.Fatal(err)
				}
			}
			cloned, err := Clone(original)
			if err != nil {
				t.Fatal(err)
			}
			cloneResult, err := cloned.Observe(10)
			if err != nil {
				t.Fatal(err)
			}
			originalResult, err := original.Observe(3)
			if err != nil {
				t.Fatal(err)
			}
			reference, err := factory.new()
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range []float64{1, 2, 3} {
				expected, err := reference.Observe(value)
				if err != nil {
					t.Fatal(err)
				}
				if value == 3 && expected != originalResult {
					t.Fatalf("original state changed through clone: got %+v, want %+v",
						originalResult, expected)
				}
			}
			if !cloneResult.Alert {
				t.Fatalf("clone did not preserve the trained detector state: %+v", cloneResult)
			}
		})
	}
}

func TestCloneRejectsUnsupportedDetector(t *testing.T) {
	if _, err := Clone(nil); err == nil {
		t.Fatal("expected unsupported detector type to be rejected")
	}
}
