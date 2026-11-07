package main

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/0002_alert_episodes.sql
var alertEpisodesMigration string

//go:embed migrations/0003_metric_rollups.sql
var metricRollupsMigration string

func applyMigrations(ctx context.Context, db *pgxpool.Pool) error {
	if _, err := db.Exec(ctx, alertEpisodesMigration); err != nil {
		return fmt.Errorf("applying alert episode migration: %w", err)
	}
	if _, err := db.Exec(ctx, metricRollupsMigration); err != nil {
		return fmt.Errorf("applying metric rollup migration: %w", err)
	}
	return nil
}
