package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	demoSeedAdvisoryLockID = 684730922
	demoBaselinePoints     = 288
	demoDriftPoints        = 36
	demoSamplesPerBucket   = 60
	demoDriftIncrease      = 12.0
)

type demoSeries struct {
	host        string
	name        string
	hostIndex   int
	metricIndex int
	mean        float64
	dailyRange  float64
}

type demoSeedResult struct {
	rawSamplesInserted int64
	rollupsInserted    int64
	driftBuckets       int
	driftStart         time.Time
}

func runDemoSeed() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	dbURL := getenv("DATABASE_URL", "postgres://pulse:pulse@localhost:5432/pulse?sslmode=disable")
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("creating database pool: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	if err := applyMigrations(ctx, pool); err != nil {
		return fmt.Errorf("applying database migrations: %w", err)
	}

	result, err := seedDemoData(ctx, pool, time.Now(), "pulse-demo")
	if err != nil {
		return err
	}
	log.Printf("demo seed added %d raw samples and %d baseline rollups; %d new drift buckets begin at %s",
		result.rawSamplesInserted, result.rollupsInserted, result.driftBuckets,
		result.driftStart.Format(time.RFC3339))
	log.Printf("the running rollup worker should create/update the drift alert within its next 30-second poll")
	return nil
}

func seedDemoData(ctx context.Context, pool *pgxpool.Pool, now time.Time, hostPrefix string) (demoSeedResult, error) {
	if strings.TrimSpace(hostPrefix) == "" {
		return demoSeedResult{}, fmt.Errorf("demo host prefix must not be empty")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return demoSeedResult{}, fmt.Errorf("starting demo seed transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var locked bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", demoSeedAdvisoryLockID).
		Scan(&locked); err != nil {
		return demoSeedResult{}, fmt.Errorf("claiming demo seed lock: %w", err)
	}
	if !locked {
		return demoSeedResult{}, fmt.Errorf("another demo seed is already running")
	}

	series := demoSeriesForPrefix(hostPrefix)
	latestEligible := floorBucket(now.Add(-rollupLatenessGrace).Add(-rollupInterval))
	candidateDriftStart := latestEligible.Add(-time.Duration(demoDriftPoints-1) * rollupInterval)
	baselineStart := candidateDriftStart.Add(-time.Duration(demoBaselinePoints) * rollupInterval)
	targetHost := series[0].host

	var firstRawSample *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT MIN(ts) FROM metrics WHERE host = $1 AND name = 'cpu.percent'`,
		targetHost).Scan(&firstRawSample); err != nil {
		return demoSeedResult{}, fmt.Errorf("checking existing demo seed history: %w", err)
	}
	if firstRawSample != nil {
		baselineStart = floorBucket(*firstRawSample)
		candidateDriftStart = baselineStart.Add(time.Duration(demoBaselinePoints) * rollupInterval)
	}
	driftWindowStart := latestEligible.Add(-time.Duration(demoDriftPoints-1) * rollupInterval)
	if driftWindowStart.Before(candidateDriftStart) {
		driftWindowStart = candidateDriftStart
	}

	var latestFinalized *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT MAX(bucket_ts) FROM metric_rollups WHERE host = $1 AND name = 'cpu.percent'`,
		targetHost).Scan(&latestFinalized); err != nil {
		return demoSeedResult{}, fmt.Errorf("checking existing demo drift rollups: %w", err)
	}
	driftStart := driftWindowStart
	if latestFinalized != nil && !latestFinalized.Add(rollupInterval).Before(driftStart) {
		driftStart = latestFinalized.Add(rollupInterval)
	}

	rawRows := make([][]any, 0, len(series)*(demoBaselinePoints+demoDriftPoints)*demoSamplesPerBucket)
	for bucket := baselineStart; bucket.Before(candidateDriftStart); bucket = bucket.Add(rollupInterval) {
		for _, metric := range series {
			rawRows = appendDemoSamples(rawRows, metric, bucket, 0)
		}
	}
	for bucket := driftWindowStart; !bucket.After(latestEligible); bucket = bucket.Add(rollupInterval) {
		for _, metric := range series {
			if metric.host == targetHost && metric.name == "cpu.percent" &&
				latestFinalized != nil && !bucket.After(*latestFinalized) {
				continue
			}
			drift := 0.0
			if metric.host == targetHost && metric.name == "cpu.percent" {
				progress := float64(bucket.Sub(candidateDriftStart)/rollupInterval+1) / demoDriftPoints
				drift = demoDriftIncrease * math.Min(progress, 1)
			}
			rawRows = appendDemoSamples(rawRows, metric, bucket, drift)
		}
	}

	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE demo_metrics (
		host TEXT NOT NULL, name TEXT NOT NULL, value DOUBLE PRECISION NOT NULL, ts TIMESTAMPTZ NOT NULL
	) ON COMMIT DROP`); err != nil {
		return demoSeedResult{}, fmt.Errorf("creating raw metric staging table: %w", err)
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"demo_metrics"},
		[]string{"host", "name", "value", "ts"}, pgx.CopyFromRows(rawRows)); err != nil {
		return demoSeedResult{}, fmt.Errorf("copying demo raw metrics: %w", err)
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO metrics (host, name, value, ts)
		 SELECT d.host, d.name, d.value, d.ts
		 FROM demo_metrics d
		 WHERE NOT EXISTS (
		   SELECT 1 FROM metrics m
		   WHERE m.host = d.host AND m.name = d.name AND m.ts = d.ts
		 )`)
	if err != nil {
		return demoSeedResult{}, fmt.Errorf("inserting demo raw metrics: %w", err)
	}

	baselineBuckets := make([]metricRollup, 0, len(series)*demoBaselinePoints)
	for bucket := baselineStart; bucket.Before(candidateDriftStart); bucket = bucket.Add(rollupInterval) {
		for _, metric := range series {
			baselineBuckets = append(baselineBuckets, metricRollup{
				key:    seriesKey{host: metric.host, name: metric.name},
				bucket: bucket,
			})
		}
	}
	// Use the worker's aggregation path: rollup values are always AVG(raw metrics).
	baselineRollups, err := aggregateMetricBuckets(ctx, tx, baselineBuckets)
	if err != nil {
		return demoSeedResult{}, fmt.Errorf("aggregating demo baseline from raw metrics: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE demo_rollups (
		host TEXT NOT NULL, name TEXT NOT NULL, bucket_ts TIMESTAMPTZ NOT NULL,
		value DOUBLE PRECISION NOT NULL, point_count BIGINT NOT NULL
	) ON COMMIT DROP`); err != nil {
		return demoSeedResult{}, fmt.Errorf("creating rollup staging table: %w", err)
	}
	rollupRows := make([][]any, 0, len(baselineRollups))
	for _, rollup := range baselineRollups {
		rollupRows = append(rollupRows, []any{
			rollup.key.host, rollup.key.name, rollup.bucket, rollup.value, rollup.pointCount,
		})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"demo_rollups"},
		[]string{"host", "name", "bucket_ts", "value", "point_count"},
		pgx.CopyFromRows(rollupRows)); err != nil {
		return demoSeedResult{}, fmt.Errorf("staging demo baseline rollups: %w", err)
	}
	rollupTag, err := tx.Exec(ctx,
		`INSERT INTO metric_rollups (host, name, bucket_ts, value, point_count)
		 SELECT host, name, bucket_ts, value, point_count FROM demo_rollups
		 ON CONFLICT (host, name, bucket_ts) DO NOTHING`)
	if err != nil {
		return demoSeedResult{}, fmt.Errorf("inserting demo baseline rollups: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return demoSeedResult{}, fmt.Errorf("committing demo seed: %w", err)
	}

	driftBuckets := 0
	if !driftStart.After(latestEligible) {
		driftBuckets = int(latestEligible.Sub(driftStart)/rollupInterval) + 1
	}
	return demoSeedResult{
		rawSamplesInserted: tag.RowsAffected(),
		rollupsInserted:    rollupTag.RowsAffected(),
		driftBuckets:       driftBuckets,
		driftStart:         driftStart,
	}, nil
}

func demoSeriesForPrefix(prefix string) []demoSeries {
	series := make([]demoSeries, 0, 6)
	for hostIndex := 1; hostIndex <= 3; hostIndex++ {
		host := fmt.Sprintf("%s-host-%d", prefix, hostIndex)
		series = append(series,
			demoSeries{host: host, name: "cpu.percent", hostIndex: hostIndex, metricIndex: 1, mean: 25, dailyRange: 2},
			demoSeries{host: host, name: "mem.percent", hostIndex: hostIndex, metricIndex: 2, mean: 55, dailyRange: 1.5},
		)
	}
	return series
}

func appendDemoSamples(rows [][]any, metric demoSeries, bucket time.Time, drift float64) [][]any {
	bucketIndex := bucket.Unix() / int64(rollupInterval.Seconds())
	seed := bucketIndex*1_000_003 + int64(metric.hostIndex*101+metric.metricIndex*17)
	random := rand.New(rand.NewSource(seed))
	dailyPhase := float64((bucketIndex%demoBaselinePoints+demoBaselinePoints)%demoBaselinePoints) / demoBaselinePoints
	level := metric.mean + metric.dailyRange*math.Sin(2*math.Pi*dailyPhase) +
		random.NormFloat64()*1.5 + drift
	for sample := 0; sample < demoSamplesPerBucket; sample++ {
		timestamp := bucket.Add(time.Duration(sample) * 5 * time.Second)
		value := level + random.NormFloat64()*0.3
		rows = append(rows, []any{metric.host, metric.name, value, timestamp})
	}
	return rows
}
