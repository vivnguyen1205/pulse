package main

import (
	"strings"
	"testing"
)

func TestEmbeddedAlertMigrationDefinesEpisodeStorage(t *testing.T) {
	for _, expected := range []string{
		"CREATE TABLE IF NOT EXISTS alert_episodes",
		"started_at",
		"ended_at",
		"peak_score",
		"last_alert_at",
		"idx_alert_episodes_active",
	} {
		if !strings.Contains(alertEpisodesMigration, expected) {
			t.Errorf("embedded migration missing %q", expected)
		}
	}
}

func TestEmbeddedRollupMigrationDefinesFiveMinuteStorage(t *testing.T) {
	for _, expected := range []string{
		"CREATE TABLE IF NOT EXISTS metric_rollups",
		"bucket_ts",
		"point_count",
		"PRIMARY KEY (host, name, bucket_ts)",
	} {
		if !strings.Contains(metricRollupsMigration, expected) {
			t.Errorf("embedded rollup migration missing %q", expected)
		}
	}
}
