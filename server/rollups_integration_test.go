package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
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

	var dbStartedAt time.Time
	var dbEndedAt *time.Time
	var dbPeakValue, dbPeakScore float64
	if err := pool.QueryRow(ctx,
		`SELECT started_at, ended_at, peak_value, peak_score
		 FROM alert_episodes WHERE host = $1 AND name = $2`,
		host, name).Scan(&dbStartedAt, &dbEndedAt, &dbPeakValue, &dbPeakScore); err != nil {
		t.Fatal(err)
	}
	fixtureStart := dbStartedAt.Add(-20 * time.Minute)
	fixtureEnd := fixtureStart.Add(5 * time.Minute)
	if _, err := pool.Exec(ctx,
		`INSERT INTO alert_episodes
		 (host, name, detector, started_at, ended_at, peak_value, peak_score, last_alert_at)
		 VALUES ($1, $2, 'closed-fixture', $3, $4, 42.5, 2.25, $4)`,
		host, name, fixtureStart, fixtureEnd); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET",
		fmt.Sprintf("/alerts?host=%s&name=%s&since=1h", host, name), nil)
	response := httptest.NewRecorder()
	server.handleAlerts(response, request)
	if response.Code != 200 {
		t.Fatalf("GET /alerts status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var apiAlerts []AlertEpisode
	if err := json.Unmarshal(response.Body.Bytes(), &apiAlerts); err != nil {
		t.Fatalf("decoding GET /alerts response: %v", err)
	}
	if len(apiAlerts) != 2 {
		t.Fatalf("GET /alerts returned %d episodes, want 2", len(apiAlerts))
	}
	var ongoingAlert, closedAlert *AlertEpisode
	for i := range apiAlerts {
		switch apiAlerts[i].Detector {
		case "ewma":
			ongoingAlert = &apiAlerts[i]
		case "closed-fixture":
			closedAlert = &apiAlerts[i]
		}
	}
	if ongoingAlert == nil || ongoingAlert.StartedAt != dbStartedAt.Unix() ||
		ongoingAlert.PeakValue != dbPeakValue || ongoingAlert.PeakScore != dbPeakScore {
		t.Fatalf("API ongoing alert %+v does not match database start/value/score (%s, %v, %v)",
			ongoingAlert, dbStartedAt, dbPeakValue, dbPeakScore)
	}
	if dbEndedAt == nil && ongoingAlert.EndedAt != nil {
		t.Fatalf("API episode ended_at = %v, want ongoing/null", ongoingAlert.EndedAt)
	}
	if dbEndedAt != nil && (ongoingAlert.EndedAt == nil || *ongoingAlert.EndedAt != dbEndedAt.Unix()) {
		t.Fatalf("API episode ended_at = %v, want %s", ongoingAlert.EndedAt, *dbEndedAt)
	}
	if closedAlert == nil || closedAlert.StartedAt != fixtureStart.Unix() ||
		closedAlert.EndedAt == nil || *closedAlert.EndedAt != fixtureEnd.Unix() ||
		closedAlert.PeakValue != 42.5 || closedAlert.PeakScore != 2.25 {
		t.Fatalf("API closed alert %+v does not match the stored fixture times and values (%s to %s, 42.5, 2.25)",
			closedAlert, fixtureStart, fixtureEnd)
	}

	if mismatches := countRollupMismatches(t, ctx, pool, host); mismatches != 0 {
		t.Fatalf("found %d rollups that do not match their raw metric bucket", mismatches)
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

func TestDemoSeedProducesAlertFromRawDrift(t *testing.T) {
	databaseURL := os.Getenv("PULSE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set PULSE_TEST_DATABASE_URL to run PostgreSQL integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
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

	prefix := fmt.Sprintf("demo-seed-test-%d", time.Now().UnixNano())
	host := prefix + "-host-1"
	defer func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM alert_episodes WHERE host LIKE $1`, prefix+"-host-%")
		_, _ = pool.Exec(context.Background(), `DELETE FROM metric_rollups WHERE host LIKE $1`, prefix+"-host-%")
		_, _ = pool.Exec(context.Background(), `DELETE FROM metrics WHERE host LIKE $1`, prefix+"-host-%")
	}()

	seedNow := time.Now()
	seedResult, err := seedDemoData(ctx, pool, seedNow, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if seedResult.rawSamplesInserted < 100_000 {
		t.Fatalf("inserted %d demo samples, want at least 100,000", seedResult.rawSamplesInserted)
	}
	if seedResult.rollupsInserted != 6*demoBaselinePoints {
		t.Fatalf("inserted %d baseline rollups, want %d",
			seedResult.rollupsInserted, 6*demoBaselinePoints)
	}

	if mismatches := countRollupMismatches(t, ctx, pool, prefix+"-host-%"); mismatches != 0 {
		t.Fatalf("found %d seeded rollups that differ from their raw-sample means", mismatches)
	}

	repeatedSeed, err := seedDemoData(ctx, pool, seedNow, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if repeatedSeed.rawSamplesInserted != 0 || repeatedSeed.rollupsInserted != 0 {
		t.Fatalf("re-running seed inserted %d raw samples and %d rollups; want 0 and 0",
			repeatedSeed.rawSamplesInserted, repeatedSeed.rollupsInserted)
	}

	server, err := newDemoTestServer(pool)
	if err != nil {
		t.Fatal(err)
	}
	// The worker processes a bounded batch per poll. Keep polling until it is
	// caught up so unrelated unrolled data in the test database cannot crowd
	// out the demo buckets.
	for poll := 0; ; poll++ {
		if poll == 20 {
			t.Fatal("rollup worker did not catch up after 20 polls")
		}
		processed, err := server.processReadyRollups(ctx, seedNow)
		if err != nil {
			t.Fatal(err)
		}
		if processed == 0 {
			break
		}
	}

	var driftRollups int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM metric_rollups
		 WHERE host = $1 AND name = 'cpu.percent' AND bucket_ts >= $2`,
		host, seedResult.driftStart).Scan(&driftRollups); err != nil {
		t.Fatal(err)
	}
	if driftRollups != seedResult.driftBuckets {
		t.Fatalf("worker finalized %d drift rollups, want %d", driftRollups, seedResult.driftBuckets)
	}

	var episodeCount int
	var dbStartedAt time.Time
	var dbEndedAt *time.Time
	var dbPeakValue, dbPeakScore float64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) OVER (), started_at, ended_at, peak_value, peak_score
		 FROM alert_episodes
		 WHERE host = $1 AND name = 'cpu.percent' AND detector = 'ewma'
		 ORDER BY started_at DESC LIMIT 1`,
		host).Scan(&episodeCount, &dbStartedAt, &dbEndedAt, &dbPeakValue, &dbPeakScore); err != nil {
		t.Fatalf("normal rollup worker did not create an EWMA episode for the seeded gradual drift: %v", err)
	}
	if dbStartedAt.Before(seedResult.driftStart) {
		t.Fatalf("episode started at %s, before the drift began at %s", dbStartedAt, seedResult.driftStart)
	}
	var otherHostEpisodes int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM alert_episodes WHERE host LIKE $1 AND host <> $2`,
		prefix+"-host-%", host).Scan(&otherHostEpisodes); err != nil {
		t.Fatal(err)
	}
	if otherHostEpisodes != 0 {
		t.Fatalf("got %d episodes on undrifted demo hosts, want 0", otherHostEpisodes)
	}

	// The dashboard reads episodes through GET /alerts; it must report the
	// stored episode exactly.
	request := httptest.NewRequest("GET",
		fmt.Sprintf("/alerts?host=%s&name=cpu.percent&since=24h", host), nil)
	response := httptest.NewRecorder()
	server.handleAlerts(response, request)
	if response.Code != 200 {
		t.Fatalf("GET /alerts status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var apiAlerts []AlertEpisode
	if err := json.Unmarshal(response.Body.Bytes(), &apiAlerts); err != nil {
		t.Fatalf("decoding GET /alerts response: %v", err)
	}
	if len(apiAlerts) != episodeCount {
		t.Fatalf("GET /alerts returned %d episodes, database has %d", len(apiAlerts), episodeCount)
	}
	got := apiAlerts[0]
	if got.Host != host || got.Name != "cpu.percent" || got.Detector != "ewma" ||
		got.StartedAt != dbStartedAt.Unix() ||
		got.PeakValue != dbPeakValue || got.PeakScore != dbPeakScore {
		t.Fatalf("API episode %+v does not match database (start %s, peak value %v, peak score %v)",
			got, dbStartedAt, dbPeakValue, dbPeakScore)
	}
	if (dbEndedAt == nil) != (got.EndedAt == nil) ||
		(dbEndedAt != nil && *got.EndedAt != dbEndedAt.Unix()) {
		t.Fatalf("API episode ended_at = %v, database ended_at = %v", got.EndedAt, dbEndedAt)
	}

	if mismatches := countRollupMismatches(t, ctx, pool, prefix+"-host-%"); mismatches != 0 {
		t.Fatalf("after worker processing, found %d rollups that differ from their raw-sample means",
			mismatches)
	}
}

// countRollupMismatches returns how many rollups for matching hosts differ from
// the mean and count of their raw samples. A rollup with no raw samples counts
// as a mismatch.
func countRollupMismatches(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hostPattern string) int {
	t.Helper()
	var mismatches int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM (
		   SELECT r.host, r.name, r.bucket_ts
		   FROM metric_rollups r
		   LEFT JOIN metrics m ON m.host = r.host AND m.name = r.name
		     AND m.ts >= r.bucket_ts AND m.ts < r.bucket_ts + INTERVAL '5 minutes'
		   WHERE r.host LIKE $1
		   GROUP BY r.host, r.name, r.bucket_ts, r.value, r.point_count
		   HAVING r.point_count <> COUNT(m.id) OR abs(r.value - AVG(m.value)) > 1e-10
		 ) mismatches`,
		hostPattern).Scan(&mismatches); err != nil {
		t.Fatal(err)
	}
	return mismatches
}

func newDemoTestServer(pool *pgxpool.Pool) (*Server, error) {
	config := anomalyConfig{
		detector:       "ewma",
		threshold:      defaultAlertThreshold,
		baselinePoints: demoBaselinePoints,
		cooldown:       defaultAlertCooldown,
	}
	if _, err := config.newDetector(); err != nil {
		return nil, err
	}
	return &Server{
		db:             pool,
		alertConfig:    config,
		detectorStates: make(map[seriesKey]*seriesState),
	}, nil
}
