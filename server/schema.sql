CREATE TABLE IF NOT EXISTS metrics (
  id    BIGSERIAL PRIMARY KEY,
  host  TEXT NOT NULL,
  name  TEXT NOT NULL,
  value DOUBLE PRECISION NOT NULL,
  ts    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_metrics_lookup ON metrics (host, name, ts);