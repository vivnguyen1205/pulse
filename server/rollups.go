package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"pulse/server/anomaly"
)

const (
	rollupAdvisoryLockID = 684730921
	rollupBatchSize      = 2000
)

type metricRollup struct {
	key        seriesKey
	bucket     time.Time
	value      float64
	pointCount int64
}

func floorBucket(timestamp time.Time) time.Time {
	return timestamp.UTC().Truncate(rollupInterval)
}

func bucketIsReady(bucket, now time.Time) bool {
	return !bucket.Add(rollupInterval).After(now.Add(-rollupLatenessGrace))
}

// runRollupWorker evaluates only closed buckets after a lateness grace period.
// It has its own serialized loop; metric ingestion does not acquire this lock.
func (s *Server) runRollupWorker(ctx context.Context) {
	ticker := time.NewTicker(rollupWorkerInterval)
	defer ticker.Stop()

	for {
		if _, err := s.processReadyRollups(ctx, time.Now()); err != nil {
			log.Printf("rollup worker failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) processReadyRollups(ctx context.Context, now time.Time) (int, error) {
	s.rollupMu.Lock()
	defer s.rollupMu.Unlock()

	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("starting rollup transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var locked bool
	if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", rollupAdvisoryLockID).
		Scan(&locked); err != nil {
		return 0, fmt.Errorf("claiming rollup worker lock: %w", err)
	}
	if !locked {
		return 0, nil
	}

	rollups, err := s.loadReadyRollups(ctx, tx, now)
	if err != nil {
		return 0, err
	}
	candidates := make(map[seriesKey]*seriesState)
	for _, rollup := range rollups {
		if _, ok := candidates[rollup.key]; ok {
			continue
		}
		if current, ok := s.detectorStates[rollup.key]; ok {
			cloned, err := anomaly.Clone(current.detector)
			if err != nil {
				return 0, fmt.Errorf("cloning detector for %s/%s: %w",
					rollup.key.host, rollup.key.name, err)
			}
			candidates[rollup.key] = &seriesState{
				detector:   cloned,
				lastBucket: current.lastBucket,
				active:     cloneEpisode(current.active),
			}
			continue
		}
		state, err := s.initializeRollupState(ctx, tx, rollup.key, rollup.bucket)
		if err != nil {
			return 0, err
		}
		candidates[rollup.key] = state
	}

	processed := 0
	for _, rollup := range rollups {
		var inserted int64
		err := tx.QueryRow(ctx,
			`INSERT INTO metric_rollups (host, name, bucket_ts, value, point_count)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (host, name, bucket_ts) DO NOTHING
			 RETURNING point_count`,
			rollup.key.host, rollup.key.name, rollup.bucket, rollup.value, rollup.pointCount).
			Scan(&inserted)
		if err == pgx.ErrNoRows {
			continue
		}
		if err != nil {
			return 0, fmt.Errorf("saving rollup for %s/%s at %s: %w",
				rollup.key.host, rollup.key.name, rollup.bucket.Format(time.RFC3339), err)
		}

		state := candidates[rollup.key]
		if !rollup.bucket.After(state.lastBucket) {
			continue
		}
		observation, err := state.detector.Observe(rollup.value)
		if err != nil {
			return 0, fmt.Errorf("detecting rollup for %s/%s at %s: %w",
				rollup.key.host, rollup.key.name, rollup.bucket.Format(time.RFC3339), err)
		}
		state.lastBucket = rollup.bucket
		change := advanceEpisode(state, rollup.bucket, observation, rollup.value, s.alertConfig.cooldown)
		if err := s.persistEpisodeChange(ctx, tx, rollup.key, state, change); err != nil {
			return 0, err
		}
		processed++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("committing rollups and alerts: %w", err)
	}
	for key, state := range candidates {
		s.detectorStates[key] = state
	}
	return processed, nil
}

func (s *Server) loadReadyRollups(ctx context.Context, tx pgx.Tx, cutoff time.Time) ([]metricRollup, error) {
	latestEligibleBucket := floorBucket(cutoff.Add(-rollupLatenessGrace).Add(-rollupInterval))
	oldestBucket := latestEligibleBucket.Add(-time.Duration(s.alertConfig.baselinePoints) * rollupInterval)
	rows, err := tx.Query(ctx,
		`SELECT ready.host, ready.name, ready.bucket_ts
		 FROM (
		   SELECT m.host, m.name,
		          date_bin('5 minutes', m.ts, TIMESTAMPTZ '2000-01-01 00:00:00+00') AS bucket_ts
		   FROM metrics m
		   WHERE date_bin('5 minutes', m.ts, TIMESTAMPTZ '2000-01-01 00:00:00+00')
		         + INTERVAL '5 minutes' <= $1
		     AND date_bin('5 minutes', m.ts, TIMESTAMPTZ '2000-01-01 00:00:00+00') >= $2
		     AND NOT EXISTS (
		       SELECT 1 FROM metric_rollups r
		       WHERE r.host = m.host AND r.name = m.name
		         AND r.bucket_ts = date_bin('5 minutes', m.ts, TIMESTAMPTZ '2000-01-01 00:00:00+00')
		     )
		   GROUP BY m.host, m.name, bucket_ts
		   ORDER BY bucket_ts, m.host, m.name
		   LIMIT $3
		 ) ready
		 ORDER BY ready.bucket_ts, ready.host, ready.name`,
		cutoff.Add(-rollupLatenessGrace), oldestBucket, rollupBatchSize)
	if err != nil {
		return nil, fmt.Errorf("querying ready metric buckets: %w", err)
	}
	defer rows.Close()

	var buckets []metricRollup
	for rows.Next() {
		var bucket metricRollup
		if err := rows.Scan(&bucket.key.host, &bucket.key.name, &bucket.bucket); err != nil {
			return nil, fmt.Errorf("scanning ready metric bucket: %w", err)
		}
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading ready metric buckets: %w", err)
	}
	return aggregateMetricBuckets(ctx, tx, buckets)
}

func aggregateMetricBuckets(ctx context.Context, tx pgx.Tx, buckets []metricRollup) ([]metricRollup, error) {
	if len(buckets) == 0 {
		return nil, nil
	}

	hosts := make([]string, len(buckets))
	names := make([]string, len(buckets))
	timestamps := make([]time.Time, len(buckets))
	for i, bucket := range buckets {
		hosts[i] = bucket.key.host
		names[i] = bucket.key.name
		timestamps[i] = bucket.bucket
	}

	rows, err := tx.Query(ctx,
		`SELECT b.host, b.name, b.bucket_ts, AVG(m.value), COUNT(*)
		 FROM unnest($1::text[], $2::text[], $3::timestamptz[]) AS b(host, name, bucket_ts)
		 JOIN metrics m ON m.host = b.host AND m.name = b.name
		   AND m.ts >= b.bucket_ts AND m.ts < b.bucket_ts + INTERVAL '5 minutes'
		 GROUP BY b.host, b.name, b.bucket_ts
		 ORDER BY b.bucket_ts, b.host, b.name`,
		hosts, names, timestamps)
	if err != nil {
		return nil, fmt.Errorf("aggregating raw metrics into five-minute rollups: %w", err)
	}
	defer rows.Close()

	rollups := make([]metricRollup, 0, len(buckets))
	for rows.Next() {
		var rollup metricRollup
		if err := rows.Scan(&rollup.key.host, &rollup.key.name, &rollup.bucket,
			&rollup.value, &rollup.pointCount); err != nil {
			return nil, fmt.Errorf("scanning aggregated metric bucket: %w", err)
		}
		rollups = append(rollups, rollup)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading aggregated metric buckets: %w", err)
	}
	return rollups, nil
}

func (s *Server) initializeRollupState(ctx context.Context, tx pgx.Tx, key seriesKey,
	before time.Time) (*seriesState, error) {
	detector, err := s.alertConfig.newDetector()
	if err != nil {
		return nil, fmt.Errorf("creating detector for %s/%s: %w", key.host, key.name, err)
	}
	state := &seriesState{detector: detector}
	rows, err := tx.Query(ctx,
		`SELECT bucket_ts, value FROM metric_rollups
		 WHERE host = $1 AND name = $2 AND bucket_ts < $3
		 ORDER BY bucket_ts DESC LIMIT $4`,
		key.host, key.name, before, s.alertConfig.baselinePoints)
	if err != nil {
		return nil, fmt.Errorf("loading rollup baseline for %s/%s: %w", key.host, key.name, err)
	}
	var timestamps []time.Time
	var values []float64
	for rows.Next() {
		var timestamp time.Time
		var value float64
		if err := rows.Scan(&timestamp, &value); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning rollup baseline for %s/%s: %w", key.host, key.name, err)
		}
		timestamps = append(timestamps, timestamp)
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("reading rollup baseline for %s/%s: %w", key.host, key.name, err)
	}
	rows.Close()
	for i := len(values) - 1; i >= 0; i-- {
		if _, err := state.detector.Observe(values[i]); err != nil {
			return nil, fmt.Errorf("warming detector for %s/%s: %w", key.host, key.name, err)
		}
	}
	if len(timestamps) > 0 {
		state.lastBucket = timestamps[0]
	}

	var episode activeEpisode
	err = tx.QueryRow(ctx,
		`SELECT id, started_at, last_alert_at, peak_value, peak_score
		 FROM alert_episodes
		 WHERE host = $1 AND name = $2 AND detector = $3 AND ended_at IS NULL
		 FOR UPDATE`,
		key.host, key.name, s.alertConfig.detector).
		Scan(&episode.id, &episode.startedAt, &episode.lastAlertAt, &episode.peakValue, &episode.peakScore)
	if err != nil && err != pgx.ErrNoRows {
		return nil, fmt.Errorf("loading active alert episode for %s/%s: %w", key.host, key.name, err)
	}
	if err == nil {
		state.active = &episode
	}
	return state, nil
}

func (s *Server) persistEpisodeChange(ctx context.Context, tx pgx.Tx, key seriesKey,
	state *seriesState, change episodeChange) error {
	if change.closeID != 0 {
		if _, err := tx.Exec(ctx,
			`UPDATE alert_episodes SET ended_at = $1 WHERE id = $2 AND ended_at IS NULL`,
			change.closeAt, change.closeID); err != nil {
			return fmt.Errorf("closing alert episode for %s/%s: %w", key.host, key.name, err)
		}
	}
	if change.create {
		if err := tx.QueryRow(ctx,
			`INSERT INTO alert_episodes
			 (host, name, detector, started_at, ended_at, peak_value, peak_score, last_alert_at)
			 VALUES ($1, $2, $3, $4, NULL, $5, $6, $4)
			 RETURNING id`,
			key.host, key.name, s.alertConfig.detector, state.active.startedAt,
			state.active.peakValue, state.active.peakScore).Scan(&state.active.id); err != nil {
			return fmt.Errorf("creating alert episode for %s/%s: %w", key.host, key.name, err)
		}
	}
	if change.update {
		if _, err := tx.Exec(ctx,
			`UPDATE alert_episodes
			 SET last_alert_at = $1, peak_value = $2, peak_score = $3
			 WHERE id = $4 AND ended_at IS NULL`,
			state.active.lastAlertAt, state.active.peakValue, state.active.peakScore, state.active.id); err != nil {
			return fmt.Errorf("updating alert episode for %s/%s: %w", key.host, key.name, err)
		}
	}
	return nil
}

func cloneEpisode(episode *activeEpisode) *activeEpisode {
	if episode == nil {
		return nil
	}
	cloned := *episode
	return &cloned
}
