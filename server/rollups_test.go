package main

import (
	"testing"
	"time"
)

func TestFloorBucketUsesFiveMinuteUTCBoundaries(t *testing.T) {
	input := time.Date(2026, 11, 7, 12, 7, 39, 0, time.FixedZone("test", -5*60*60))
	got := floorBucket(input)
	want := time.Date(2026, 11, 7, 17, 5, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("floorBucket = %s, want %s", got, want)
	}
}

func TestBucketIsReadyOnlyAfterBucketAndGraceHavePassed(t *testing.T) {
	bucket := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"inside bucket", bucket.Add(4*time.Minute + 59*time.Second), false},
		{"during grace", bucket.Add(5*time.Minute + 59*time.Second), false},
		{"grace boundary", bucket.Add(6 * time.Minute), true},
		{"after grace", bucket.Add(7 * time.Minute), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bucketIsReady(bucket, tt.now); got != tt.want {
				t.Fatalf("bucketIsReady = %v, want %v", got, tt.want)
			}
		})
	}
}
