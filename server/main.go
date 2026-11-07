package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Metric is one data point sent by an agent.
type Metric struct {
	Host      string  `json:"host"`
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	Timestamp int64   `json:"timestamp"`
}

// Point is one (time, value) pair returned by the query API.
type Point struct {
	Timestamp int64   `json:"timestamp"`
	Value     float64 `json:"value"`
}

// QueryResponse is what GET /metrics returns.
type QueryResponse struct {
	Host   string  `json:"host"`
	Name   string  `json:"name"`
	Points []Point `json:"points"`
}

// AlertEpisode is one deduplicated interval of anomalous metric samples.
type AlertEpisode struct {
	ID        int64   `json:"id"`
	Host      string  `json:"host"`
	Name      string  `json:"name"`
	Detector  string  `json:"detector"`
	StartedAt int64   `json:"started_at"`
	EndedAt   *int64  `json:"ended_at"`
	PeakValue float64 `json:"peak_value"`
	PeakScore float64 `json:"peak_score"`
}

type Server struct {
	db             *pgxpool.Pool
	apiKey         string // required on POST /metrics when set
	allowedOrigin  string // which website may call GET /metrics from a browser
	alertConfig    anomalyConfig
	rollupMu       sync.Mutex
	detectorStates map[seriesKey]*seriesState
}

func main() {
	dbURL := getenv("DATABASE_URL", "postgres://pulse:pulse@localhost:5432/pulse")
	port := getenv("PORT", "8080") // hosting platforms set PORT for you
	apiKey := os.Getenv("PULSE_API_KEY")
	allowedOrigin := getenv("ALLOWED_ORIGIN", "*")
	alertConfig, err := anomalyConfigFromEnv()
	if err != nil {
		log.Fatalf("invalid anomaly detection configuration: %v", err)
	}

	if apiKey == "" {
		log.Println("WARNING: PULSE_API_KEY is not set; anyone can send metrics")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("could not create db pool: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("cannot reach database: %v", err)
	}
	if err := applyMigrations(ctx, pool); err != nil {
		log.Fatalf("cannot apply database migrations: %v", err)
	}

	s := &Server{
		db:             pool,
		apiKey:         apiKey,
		allowedOrigin:  allowedOrigin,
		alertConfig:    alertConfig,
		detectorStates: make(map[seriesKey]*seriesState),
	}
	go s.runRollupWorker(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("POST /metrics", s.handleIngest)
	mux.HandleFunc("GET /metrics", s.handleQuery)
	mux.HandleFunc("GET /alerts", s.handleAlerts)
	mux.HandleFunc("GET /healthz", s.handleHealth)

	log.Printf("server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// getenv returns the environment variable, or a fallback if it's not set.
func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// handleHealth lets the hosting platform check that the server is alive.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		http.Error(w, "database unreachable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

// handleIngest receives metrics from agents and stores them.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if s.apiKey != "" {
		got := r.Header.Get("Authorization")
		want := "Bearer " + s.apiKey
		// Constant-time comparison so attackers can't guess the key by timing responses.
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Reject huge request bodies (1 MB max) so nobody can exhaust server memory.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var metrics []Metric
	if err := json.NewDecoder(r.Body).Decode(&metrics); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	if err := validateMetrics(metrics); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	rows := make([][]any, len(metrics))
	for i, metric := range metrics {
		rows[i] = []any{metric.Host, metric.Name, metric.Value, time.Unix(metric.Timestamp, 0)}
	}
	// Keep ingestion on the original batched COPY path. Rollups and detection
	// run independently in the background so incoming batches are not serialized.
	if _, err := s.db.CopyFrom(r.Context(), pgx.Identifier{"metrics"},
		[]string{"host", "name", "value", "ts"}, pgx.CopyFromRows(rows)); err != nil {
		log.Printf("batch insert failed: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusAccepted)
}

// handleAlerts lists recent alert episodes. Optional filters are host and name;
// since defaults to 24h and limit defaults to 100 (maximum 500).
func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", s.allowedOrigin)

	q := r.URL.Query()
	since := 24 * time.Hour
	if value := q.Get("since"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			http.Error(w, "invalid since (use e.g. 15m, 1h, 24h)", http.StatusBadRequest)
			return
		}
		since = duration
	}
	limit := 100
	if value := q.Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 500 {
			http.Error(w, "limit must be an integer from 1 to 500", http.StatusBadRequest)
			return
		}
		limit = parsed
	}

	rows, err := s.db.Query(r.Context(),
		`SELECT id, host, name, detector, started_at, ended_at, peak_value, peak_score
		 FROM alert_episodes
		 WHERE started_at >= $1
		   AND ($2 = '' OR host = $2)
		   AND ($3 = '' OR name = $3)
		 ORDER BY started_at DESC
		 LIMIT $4`,
		time.Now().Add(-since), q.Get("host"), q.Get("name"), limit)
	if err != nil {
		log.Printf("alert query failed: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	alerts := []AlertEpisode{}
	for rows.Next() {
		var alert AlertEpisode
		var startedAt time.Time
		var endedAt *time.Time
		if err := rows.Scan(&alert.ID, &alert.Host, &alert.Name, &alert.Detector,
			&startedAt, &endedAt, &alert.PeakValue, &alert.PeakScore); err != nil {
			log.Printf("alert scan failed: %v", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		alert.StartedAt = startedAt.Unix()
		if endedAt != nil {
			endedAtUnix := endedAt.Unix()
			alert.EndedAt = &endedAtUnix
		}
		alerts = append(alerts, alert)
	}
	if err := rows.Err(); err != nil {
		log.Printf("alert rows error: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(alerts); err != nil {
		log.Printf("alert response encoding failed: %v", err)
	}
}

// handleQuery returns stored metrics for one host and metric name over a time window.
// Example: GET /metrics?host=my-laptop&name=cpu.percent&since=1h
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", s.allowedOrigin)

	q := r.URL.Query()
	host := q.Get("host")
	name := q.Get("name")
	if host == "" || name == "" {
		http.Error(w, "host and name query parameters are required", http.StatusBadRequest)
		return
	}

	since := time.Hour
	if v := q.Get("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			http.Error(w, "invalid since (use e.g. 15m, 1h, 24h)", http.StatusBadRequest)
			return
		}
		since = d
	}

	rows, err := s.db.Query(r.Context(),
		`SELECT ts, value FROM metrics
		 WHERE host = $1 AND name = $2 AND ts >= $3
		 ORDER BY ts`,
		host, name, time.Now().Add(-since))
	if err != nil {
		log.Printf("query failed: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	points := []Point{} // start empty (not nil) so JSON is [] instead of null
	for rows.Next() {
		var ts time.Time
		var value float64
		if err := rows.Scan(&ts, &value); err != nil {
			log.Printf("scan failed: %v", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		points = append(points, Point{Timestamp: ts.Unix(), Value: value})
	}
	if err := rows.Err(); err != nil {
		log.Printf("rows error: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(QueryResponse{Host: host, Name: name, Points: points})
}
