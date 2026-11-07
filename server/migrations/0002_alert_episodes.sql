CREATE TABLE IF NOT EXISTS alert_episodes (
  id            BIGSERIAL PRIMARY KEY,
  host          TEXT NOT NULL,
  name          TEXT NOT NULL,
  detector      TEXT NOT NULL,
  started_at    TIMESTAMPTZ NOT NULL,
  ended_at      TIMESTAMPTZ,
  peak_value    DOUBLE PRECISION NOT NULL,
  peak_score    DOUBLE PRECISION NOT NULL,
  last_alert_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_alert_episodes_lookup
  ON alert_episodes (host, name, started_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_alert_episodes_active
  ON alert_episodes (host, name, detector)
  WHERE ended_at IS NULL;
