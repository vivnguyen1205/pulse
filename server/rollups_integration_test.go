package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestRollupWorkerIntegration(t *testing.T) {
	databaseURL := os.Getenv("PULSE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set PULSE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS metrics (
		id BIGSERIAL PRIMARY KEY, host TEXT NOT NULL, name TEXT NOT NULL,
		value DOUBLE PRECISION NOT NULL, ts TIMESTAMPTZ NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_metrics_lookup ON metrics (host, name, ts)`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(ctx, pool); err != nil {
		t.Fatal(err)
	}

	host := fmt.Sprintf("rollup-integration-%d", time.Now().UnixNano())
	name := "cpu.percent"
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM alert_episodes WHERE host = $1`, host)
		_, _ = pool.Exec(context.Background(), `DELETE FROM metric_rollups WHERE host = $1`, host)
		_, _ = pool.Exec(context.Background(), `DELETE FROM metrics WHERE host = $1`, host)
	}()

	latestEligibleBucket := floorBucket(time.Now().Add(-rollupLatenessGrace).Add(-rollupInterval))
	firstBucket := latestEligibleBucket.Add(-3 * rollupInterval)
	for bucketIndex, value := range []float64{9, 10, 11, 50} {
		for point := 0; point < 2; point++ {
			timestamp := firstBucket.Add(time.Duration(bucketIndex) * rollupInterval).
				Add(time.Duration(point) * time.Minute)
			if _, err := pool.Exec(ctx,
				`INSERT INTO metrics (host, name, value, ts) VALUES ($1, $2, $3, $4)`,
				host, name, value, timestamp); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO metrics (host, name, value, ts) VALUES ($1, $2, $3, $4)`,
			host, name, 99.0, floorBucket(time.Now()).Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	server := &Server{
		db: pool,
		alertConfig: anomalyConfig{
			detector:       "ewma",
			threshold:      2.8,
			baselinePoints: 3,
			cooldown:       15 * time.Minute,
		},
		detectorStates: make(map[seriesKey]*seriesState),
	}
	processed, err := server.processReadyRollups(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if processed != 4 {
		t.Fatalf("processed %d rollups, want 4", processed)
	}

	var value float64
	var pointCount int64
	err = pool.QueryRow(ctx,
		`SELECT value, point_count FROM metric_rollups
		 WHERE host = $1 AND name = $2 ORDER BY bucket_ts DESC LIMIT 1`,
		host, name).Scan(&value, &pointCount)
	if err != nil {
		t.Fatal(err)
	}
	if value != 50 || pointCount != 2 {
		t.Fatalf("final rollup = value %v, count %d; want 50 and 2", value, pointCount)
	}
	var rollupCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM metric_rollups WHERE host = $1`, host).
		Scan(&rollupCount); err != nil {
		t.Fatal(err)
	}
	if rollupCount != 4 {
		t.Fatalf("got %d rollups; partial current bucket must not be evaluated", rollupCount)
	}

	var alertCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_episodes WHERE host = $1 AND name = $2`,
		host, name).Scan(&alertCount); err != nil {
		t.Fatal(err)
	}
	if alertCount != 1 {
		t.Fatalf("got %d alert episodes, want 1", alertCount)
	}

	processed, err = server.processReadyRollups(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if processed != 0 {
		t.Fatalf("second worker pass processed %d rollups, want 0", processed)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO metrics (host, name, value, ts) VALUES ($1, $2, $3, $4)`,
		host, name, 99.0, firstBucket.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	processed, err = server.processReadyRollups(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if processed != 0 {
		t.Fatalf("late point changed %d finalized rollups, want 0", processed)
	}
	err = pool.QueryRow(ctx,
		`SELECT value, point_count FROM metric_rollups
		 WHERE host = $1 AND name = $2 AND bucket_ts = $3`,
		host, name, firstBucket).Scan(&value, &pointCount)
	if err != nil {
		t.Fatal(err)
	}
	if value != 9 || pointCount != 2 {
		t.Fatalf("late point revised finalized rollup: value %v count %d", value, pointCount)
	}
}
