
# Pulse
 
A lightweight observability platform built from scratch in Go. Agents collect system metrics from machines and send them to a central server, which stores them for querying and visualization.
 
## Architecture
 
```
[Agent (Go)] ──HTTP POST /metrics──> [Server (Go)] ──> [PostgreSQL]
                                          │
                       rollup worker: five-minute averages
                         → detector → alert episodes
                                          │
[Dashboard (Next.js)] <──GET /metrics, GET /alerts
```
 
- **Agent** (`agent/`): runs on each machine and collects CPU and memory usage every 5 seconds using [gopsutil](https://github.com/shirou/gopsutil). Sends metrics to the server as JSON.
- **Server** (`server/`): HTTP API that validates and stores incoming metrics, plus a background worker that evaluates five-minute rollups and stores alert episodes.
- **Database**: PostgreSQL running in Docker, with a `metrics` table indexed on `(host, name, ts)` for time-range queries, plus `metric_rollups` (five-minute averages) and `alert_episodes`.
- **Dashboard** (`dashboard/`): Next.js charts of CPU and memory per host with alert episodes shaded, plus an Alerts page.
## Getting started
 
**Requirements:** Go 1.22+, Docker
 
1. Start the database:
```bash
   docker compose up -d
```
 
2. Start the server:
```bash
   cd server
   go run .
```
 
3. Start the agent (in a new terminal):
```bash
   cd agent
   go run .
```
 
4. Verify data is arriving:
```bash
   docker compose exec db psql -U pulse -d pulse -c "SELECT * FROM metrics ORDER BY ts DESC LIMIT 10;"
```

5. Start the dashboard (in a new terminal), then open http://localhost:3000:
```bash
   cd dashboard
   npm install
   NEXT_PUBLIC_PULSE_API=http://localhost:8080 npm run dev
```
 
### Configuration
 
| Variable | Used by | Default |
|---|---|---|
| `PULSE_SERVER` | agent | `http://localhost:8080` |
| `PULSE_DETECTOR` | server | `ewma` (`ewma`, `rolling-zscore`, or `cusum`) |
| `PULSE_DETECTOR_THRESHOLD` | server | Detector-specific; EWMA defaults to calibrated `3.8956` |
| `PULSE_DETECTOR_BASELINE_POINTS` | server | `288` |
| `PULSE_ALERT_COOLDOWN` | server | `15m` |
| `DATABASE_URL` | server | `postgres://pulse:pulse@localhost:5432/pulse` |
 
See [Anomaly detection](#anomaly-detection) for what the detector settings mean
and how the defaults were chosen.

## Anomaly detection

Pulse watches each host's CPU and memory and raises an alert when a metric
starts behaving differently from its own recent history. No fixed limits like
"alert above 90%" are needed: each host/metric pair learns what normal looks
like for itself.

### How it works, step by step

1. **Raw samples.** The agent sends a CPU and a memory reading every five
   seconds. They are stored unchanged in `metrics`.
2. **Five-minute averages.** A background worker averages each host/metric's
   samples into five-minute buckets (`metric_rollups`). One five-second
   reading is noisy; the average of about 60 readings is much steadier, so the
   detector reacts to real changes rather than momentary blips.
3. **Learn what's normal.** The detector uses the first 288 five-minute
   averages it sees (24 hours) as its **baseline**: the typical level and the
   typical size of random wobble (the *standard deviation*, written σ below).
4. **Score each new average.** Every new five-minute average gets a score
   saying how far from normal things look. Above the threshold, it is flagged.
5. **Group flags into episodes.** Flags that arrive within 15 minutes of each
   other are merged into one **alert episode** (`alert_episodes`) with a start,
   an end, and its worst ("peak") value and score. One ongoing problem
   therefore produces one alert, not hundreds.
6. **Show it.** The dashboard shades episodes on the charts and lists them on
   the Alerts page, both via `GET /alerts`.

### The three detectors

Pulse includes three detectors (`server/anomaly`). They answer slightly
different questions, which is why they are good at different problems.

**Rolling z-score: "How unusual is this one point?"**
It takes the average and wobble (σ) of the last 288 points, then measures how
many σ the new point is from that average. That number is the *z-score*. With
the default threshold of 3, a point more than 3σ away is flagged.
- Good at: sudden spikes and big jumps.
- Weak at: slow drift. The window keeps moving, so a slowly rising metric
  drags the "normal" average up with it and never looks unusual. After a
  sustained shift, the shift also gradually becomes the new normal.

**EWMA (exponentially weighted moving average): "Is the trend moving away?"**
It keeps a smoothed running average in which the newest point counts for 20%
(`alpha = 0.2`) and older points fade out. Random ups and downs cancel out in
the smoothed line, so a small but consistent change stands out. The smoothed
line is compared with the baseline average; the score is its distance as a
fraction of an allowed band (threshold × σ, narrowed to account for the
smoothing). A score above 1 is an alert.
- Good at: steps and gradual drift; still catches most large spikes.
- Trade-off: a higher `alpha` reacts faster but lets more noise through.

**CUSUM (cumulative sum): "Is there steady evidence of a shift?"**
It keeps a running tally of suspicion. Each point adds how many σ it sits
above (or below, in a separate tally) the baseline, minus an *allowance* of
0.5σ so ordinary wobble drains the tally instead of filling it. The tally can't
go below zero. When it passes the *decision interval* (threshold), it alerts
and resets.
- Good at: small shifts that persist; the most reliable for gradual drift.
- Weak at: single spikes. One point adds only a few σ, not enough to pass the
  threshold on its own.

| Detector | Setting | Pulse value | Raising it means |
|---|---|---|---|
| All | Baseline points (`PULSE_DETECTOR_BASELINE_POINTS`) | 288 (24 h) | Steadier sense of normal; longer warm-up |
| Rolling z-score | Threshold (σ) | 3 | Fewer false alarms; misses smaller problems |
| EWMA | `alpha` | 0.2 | Faster reaction; noisier |
| EWMA | Threshold (band width in σ) | 3.8956 | Fewer false alarms; slower, less sensitive |
| CUSUM | Allowance (σ) | 0.5 | Ignores smaller shifts |
| CUSUM | Decision interval | 5 | Fewer false alarms; slower detection |

Note that EWMA and CUSUM learn their baseline once (during warm-up) and keep
it; the rolling z-score's baseline moves with time.

### How detection was tested (back-testing)

You can't judge a detector by watching a few real incidents, because you never
know exactly when a real problem began or how many were missed. So the harness
in `server/anomaly/backtest` generates **synthetic series where the right
answer is known**, then measures how each detector does.

**The fake data.** Each series is a stream of five-minute averages around 50
(think "50% CPU"), with random noise (σ = 2) and a mild daily up-and-down cycle
(±1). The first 288 points (one day) are normal, for warm-up. Then one of three
faults is injected at a known point:

| Fault | What is injected | Size | Counts as detected if flagged within |
|---|---|---|---|
| Step shift | Level jumps and stays up | +6 (3σ) | 12 points (1 hour) |
| Gradual drift | Level climbs steadily | 0 → +6 over 24 hours | 144 points (12 hours) |
| Single spike | One point jumps | +10 (5σ) | That point only |

A fourth set of **control** series has no fault at all, which is how false
alarms are measured.

**Fair comparison.** Each detector's threshold changes both how much it catches
and how often it cries wolf, so comparing default settings is unfair. The
harness first tunes each detector's threshold on 200 separate fault-free series
(calibration seed `8820261107`) until it flags about **2 points per 1,000
normal points**. It then evaluates every detector on fresh series: 200 per
fault type for each of 5 seeds (1,000 per fault type in total). Everything uses
fixed random seeds, so rerunning the harness gives the same numbers.

Rerun it yourself (about 15 seconds):

```bash
cd server
go run ./cmd/anomaly-backtest
```

Flags such as `-trials`, `-seeds`, and `-target-false-alarms` change the setup.

### Results

At a matched false-alarm rate (each detector tuned to about 2 false alarm
points per 1,000 normal points):

| Detector | Fault | Timely detection rate | False alarm points / 1,000 normal points | False alarm episodes / normal day | Pooled median delay [seed min–max] (points) |
|---|---|---:|---:|---:|---:|
| Rolling z-score | step shift | 96.0% [94.5%–97.5%] | 1.7 [1.5–1.9] | 0.5 [0.4–0.6] | 1.0 [1.0–1.0] |
| Rolling z-score | gradual drift | 47.4% [41.5%–51.0%] | 1.7 [1.5–1.9] | 0.5 [0.4–0.6] | 87.5 [78.0–97.0] |
| Rolling z-score | single spike | 93.9% [92.0%–96.0%] | 1.7 [1.5–1.9] | 0.5 [0.4–0.6] | 0.0 [0.0–0.0] |
| EWMA | step shift | 100.0% [100.0%–100.0%] | 1.8 [1.5–2.0] | 0.4 [0.3–0.4] | 1.0 [1.0–2.0] |
| EWMA | gradual drift | 93.5% [92.0%–95.5%] | 1.8 [1.5–2.0] | 0.4 [0.3–0.4] | 83.0 [78.0–86.5] |
| EWMA | single spike | 79.7% [78.0%–82.0%] | 1.8 [1.5–2.0] | 0.4 [0.3–0.4] | 0.0 [0.0–0.0] |
| CUSUM | step shift | 100.0% [100.0%–100.0%] | 1.8 [1.6–2.0] | 0.5 [0.5–0.6] | 4.0 [4.0–4.0] |
| CUSUM | gradual drift | 97.9% [97.0%–99.0%] | 1.8 [1.6–2.0] | 0.5 [0.5–0.6] | 77.0 [75.0–80.5] |
| CUSUM | single spike | 0.0% [0.0%–0.0%] | 1.8 [1.6–2.0] | 0.5 [0.5–0.6] | — |

Fault-free control series at the same settings:

| Detector | Tuned threshold | Chance of any alert in control series | False alarm points / 1,000 normal points | False alarm episodes / normal day |
|---|---:|---:|---:|---:|
| Rolling z-score | 3.1441 | 39.7% [36.0%–44.5%] | 1.7 [1.5–1.9] | 0.5 [0.4–0.6] |
| EWMA | 3.8956 | 27.1% [24.0%–30.5%] | 1.8 [1.5–2.0] | 0.4 [0.3–0.4] |
| CUSUM | 10.0022 | 42.4% [37.0%–48.0%] | 1.8 [1.6–2.0] | 0.5 [0.5–0.6] |

<details>
<summary>Results at each detector's untuned default threshold</summary>

Defaults: rolling z-score 3, EWMA 3.5, CUSUM 5 (EWMA in Pulse uses the tuned
3.8956 instead). Thresholds differ in how strict they are, so these rows are
not a like-for-like comparison.

| Detector | Fault | Timely detection rate | False alarm points / 1,000 normal points | False alarm episodes / normal day | Pooled median delay [seed min–max] (points) |
|---|---|---:|---:|---:|---:|
| Rolling z-score | step shift | 98.3% [97.0%–99.5%] | 2.7 [2.4–3.0] | 0.8 [0.7–0.9] | 1.0 [0.0–1.0] |
| Rolling z-score | gradual drift | 65.3% [62.5%–68.0%] | 2.7 [2.4–3.0] | 0.8 [0.7–0.9] | 82.0 [74.0–94.0] |
| Rolling z-score | single spike | 94.6% [94.0%–96.5%] | 2.7 [2.4–3.0] | 0.8 [0.7–0.9] | 0.0 [0.0–0.0] |
| EWMA | step shift | 100.0% [100.0%–100.0%] | 6.0 [5.6–6.2] | 1.1 [1.0–1.2] | 1.0 [1.0–1.0] |
| EWMA | gradual drift | 98.1% [97.5%–98.5%] | 6.0 [5.6–6.2] | 1.1 [1.0–1.2] | 74.0 [69.0–78.0] |
| EWMA | single spike | 88.6% [86.5%–93.0%] | 6.0 [5.6–6.2] | 1.1 [1.0–1.2] | 0.0 [0.0–0.0] |
| CUSUM | step shift | 100.0% [100.0%–100.0%] | 10.5 [10.2–10.7] | 3.0 [3.0–3.1] | 2.0 [2.0–2.0] |
| CUSUM | gradual drift | 100.0% [100.0%–100.0%] | 10.5 [10.2–10.7] | 3.0 [3.0–3.1] | 49.0 [47.0–51.0] |
| CUSUM | single spike | 23.2% [21.0%–26.5%] | 10.5 [10.2–10.7] | 3.0 [3.0–3.1] | 0.0 [0.0–0.0] |

| Detector | Chance of any alert in control series | False alarm points / 1,000 normal points | False alarm episodes / normal day |
|---|---:|---:|---:|
| Rolling z-score | 55.6% [50.5%–62.5%] | 2.7 [2.4–3.0] | 0.8 [0.7–0.9] |
| EWMA | 57.4% [53.5%–61.5%] | 6.0 [5.6–6.2] | 1.1 [1.0–1.2] |
| CUSUM | 96.7% [94.5%–98.0%] | 10.5 [10.2–10.7] | 3.0 [3.0–3.1] |

</details>

### How to read the results

- **Timely detection rate**: out of 1,000 series with a fault, the share where
  the detector's first alert landed inside the fault's window (table above).
  Higher is better.
- **False alarm points / 1,000 normal points**: in healthy data, how many of
  every 1,000 five-minute averages were wrongly flagged. 1,000 points is about
  3.5 days of one metric on one host. Lower is better.
- **False alarm episodes / normal day**: the same, but counting runs of
  back-to-back false flags as one, per 24 hours of one metric. This is closer
  to "how many times would I be bothered?"
- **Chance of any alert in control series**: how often a completely healthy
  day of one metric produced at least one false alert.
- **Median delay**: how many five-minute points passed between the fault
  starting and the alert, among detected faults. 0 means flagged on the very
  first faulty point; 1 point = 5 minutes.
- **[min–max]**: the spread across the 5 independent seeds. A narrow range
  means the number is stable, not a lucky draw.

### Which detector suits which fault

| Fault | Best choice | Why (matched-rate results) |
|---|---|---|
| Sudden step | EWMA or CUSUM | Both catch 100%; EWMA within about 5 minutes (median 1 point), CUSUM about 20 minutes (4 points). Rolling z-score catches 96.0%. |
| Gradual drift | CUSUM, then EWMA | CUSUM catches 97.9% (median about 6.4 hours); EWMA 93.5% (about 6.9 hours). Rolling z-score only 47.4%, because the drift becomes part of its moving baseline. |
| Single spike | Rolling z-score | Catches 93.9%; EWMA 79.7% (smoothing dilutes one point). CUSUM catches none: one point can't fill its tally past the tuned threshold. |

**Why Pulse defaults to EWMA:** it is the only detector that catches most of
every fault type at the same false-alarm rate (100% / 93.5% / 79.7%), and it
had the lowest chance of a false alert on a healthy day (27.1%). Its default
threshold is the tuned 3.8956, so the **EWMA rows in the matched-rate table
describe Pulse's default configuration**.

Gradual drift is slow to detect for every detector. By EWMA's median delay
(83 points, about 7 hours), the drift has only reached about 1.75 above normal,
less than one σ of noise. Catching it earlier would mean more false alarms.

### How it runs inside Pulse

Detection runs in a separate background worker, not on each incoming request.
The agent keeps sending raw samples every five seconds. The worker computes the
mean for each host/metric five-minute bucket and evaluates a bucket only after
it ends and a one-minute lateness grace has passed. The worker polls every 30
seconds, so in normal operation an alert is evaluated about one to
one-and-a-half minutes after its bucket closes. An unfinished final bucket is
never evaluated early.

**Trade-off:** checking on every incoming request would alert sooner, but it
would put detector work and locking on the high-throughput ingest path.
Ingestion keeps its batched `COPY` path and never waits on detection. The cost
is up to about 1.5 minutes of extra detection delay and a separately polled
worker. A worker-only lock and a PostgreSQL advisory lock make sure only one
server replica processes buckets at a time.

Points received before the grace period ends are included in the bucket mean,
including out-of-order timestamps. Once a bucket is saved to `metric_rollups`,
it is final: later-arriving points are still stored in `metrics` but do not
revise the rollup or retroactively change alerts. The worker only scans the
latest baseline-sized horizon, so after a long outage it warms up on recent
rollups instead of replaying unbounded old history.

Detector warm-up and state are counted in **rollup points**, not raw samples,
so the default 288-point baseline is 24 hours. The detector's live state is
held in server memory. On first use or after a restart, the worker replays the
most recent stored rollups to warm the detector up, and restores any active
alert episode from PostgreSQL.

Alerts are stored as **episodes**, not one row per flagged point. Flags within
the 15-minute cooldown (`PULSE_ALERT_COOLDOWN`) extend the same episode and
update its peak score and value. An episode is closed when a later point
arrives after the cooldown; its end time is the last anomalous bucket. An
ongoing episode has no end time until then. Longer cooldowns reduce noise but
merge separate nearby incidents. `peak_score` means something different per
detector: EWMA's is distance relative to its allowed band (above 1 alerts),
the z-score's is in σ, and CUSUM's is its accumulated tally.

### Known limitations

- **Synthetic test data only.** The numbers come from generated series with
  smooth, evenly random noise. Real CPU and memory are burstier, are bounded
  between 0 and 100%, change with weekly cycles and deploys, and have noise
  that sticks around (a busy minute tends to be followed by another). Expect
  more false alarms on real hosts than the table shows.
- **False alarms add up across hosts.** About 0.4 false episodes per normal day
  is *per metric per host*. With 10 hosts × 2 metrics, that is roughly 8 false
  episodes a day in the back-test model. The 15-minute cooldown merges a bit
  more than the back-test does, but the rough scale holds.
- **Thresholds were tuned once, on synthetic data.** 3.8956 is a starting
  point, not a guarantee. The evaluation runs landed at 1.7–1.8 false alarm
  points per 1,000 rather than exactly 2, because tuning and evaluation used
  different random data.
- **Non-default detectors use untuned thresholds.** `PULSE_DETECTOR=cusum`
  uses threshold 5, which in the back-test produced a false alert on 96.7% of
  healthy days. To match the tuned rate, set `PULSE_DETECTOR_THRESHOLD` to
  `10.0022` (CUSUM) or `3.1441` (rolling z-score).
- **EWMA and CUSUM never update their baseline.** A permanent, legitimate change
  (for example, a host that now runs a heavier workload) keeps looking
  abnormal until the server restarts and re-learns from recent rollups.
- **Short spikes get averaged away.** A burst shorter than five minutes is
  diluted by the five-minute average, and the fastest possible alert comes
  after the bucket closes plus the grace period and poll (about 6–7 minutes).
- **Drift is caught late.** Median detection takes about 6–7 hours for the
  slow drift tested here.
- **One detector at a time.** Pulse runs the configured detector only; it does
  not combine them (for example, z-score for spikes plus CUSUM for drift).
- **A new host needs 24 hours** of data before it can alert at all.
- **Very late samples are ignored for detection** once their bucket has been
  finalized.

## API
 
### `POST /metrics`
 
Accepts a JSON array of metrics. Returns `202 Accepted` on success.
 
```json
[
  { "host": "my-laptop", "name": "cpu.percent", "value": 42.5, "timestamp": 1792952826 }
]
```

### `GET /alerts`

Lists recent alert episodes. Optional filters are `host`, `name`, `since` (Go
duration, default `24h`), and `limit` (1–500, default `100`).

```text
GET /alerts?host=my-laptop&name=cpu.percent&since=24h
```

Episodes that overlap the requested time range are included, even if they
started before `since`.

The dashboard links to an **Alerts** page for recent episodes and filters by
host and metric. The CPU and memory charts shade matching episodes from the
start of the first anomalous five-minute bucket through the end of the last
one; ongoing shading extends to the current time. Hover over a shaded region for its
detector, peak value, and peak score.

### Demo data

Run the server against the target database before seeding, so its rollup worker
can evaluate the injected drift. The command writes five-second raw samples
for three demo hosts across roughly one day of baseline history and a gradual
CPU drift, then derives the baseline rollups from those raw samples with the
same five-minute bucketing and `AVG` aggregation used by the worker. It never
writes alert rows; the running worker creates the real alert episode. Sample
timestamps and values are deterministic, duplicate raw timestamps are skipped,
and existing rollups are not overwritten, so rerunning the command is safe.

For local Docker Postgres:

```bash
docker compose up -d db
cd server
go run .
```

In another terminal:

```bash
cd server
DATABASE_URL='postgres://pulse:pulse@localhost:5432/pulse?sslmode=disable' go run . demo-seed
```

For Neon, use the same command with the database URL of the branch connected to
the running Pulse server (prefer Neon’s direct connection string for the bulk
COPY operation):

```bash
cd server
DATABASE_URL="$NEON_DATABASE_URL" go run . demo-seed
```

The worker should pick up the already-complete drift buckets on its next
30-second poll, so the alert normally appears within about 30 seconds after
the seed command finishes. Select a demo host (for example,
`pulse-demo-host-1`) and the `24h` chart range to see its seeded history.

Reruns keep the original baseline window and only add missing buckets, so a
rerun after the drift has already been evaluated will not necessarily raise a
new alert. To replay the demo from scratch, delete the demo hosts' rows first
(this removes only `pulse-demo-host-*` data):

```sql
DELETE FROM alert_episodes WHERE host LIKE 'pulse-demo-host-%';
DELETE FROM metric_rollups WHERE host LIKE 'pulse-demo-host-%';
DELETE FROM metrics WHERE host LIKE 'pulse-demo-host-%';
```

Then restart the server (it caches detector state in memory) and rerun the seed.

The server applies the idempotent alert-episode migration at startup. The same
schema is included in `server/schema.sql` for new local databases. To run the
PostgreSQL rollup, seed, and alert-API integration tests locally:

```bash
docker compose up -d db
cd server
PULSE_TEST_DATABASE_URL='postgres://pulse:pulse@localhost:5432/pulse?sslmode=disable' \
  go test -run '^(TestRollupWorkerIntegration|TestDemoSeedProducesAlertFromRawDrift)$' -v .
```

To compare ingest throughput against a 20K+ points/second target, start the
server (`cd server && go run .`) and run the load generator in another
terminal:

```bash
cd loadgen
go run . -url http://localhost:8080 -agents 2000 -metrics 10 -interval 1s -duration 30s
```

If the server requires `PULSE_API_KEY`, also pass `-key "$PULSE_API_KEY"`.
Compare the reported actual points/second to the previous benchmark using the
same machine and settings; the 20K target is not guaranteed by this command.
 
