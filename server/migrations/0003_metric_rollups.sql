CREATE TABLE IF NOT EXISTS metric_rollups (
  host        TEXT NOT NULL,
  name        TEXT NOT NULL,
  bucket_ts   TIMESTAMPTZ NOT NULL,
  value       DOUBLE PRECISION NOT NULL,
  point_count BIGINT NOT NULL,
  PRIMARY KEY (host, name, bucket_ts)
);

CREATE INDEX IF NOT EXISTS idx_metric_rollups_recent
  ON metric_rollups (host, name, bucket_ts DESC);
