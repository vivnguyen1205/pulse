package main
 
import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
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
 
type Server struct {
	db            *pgxpool.Pool
	apiKey        string // required on POST /metrics when set
	allowedOrigin string // which website may call GET /metrics from a browser
}
 
func main() {
	dbURL := getenv("DATABASE_URL", "postgres://pulse:pulse@localhost:5432/pulse")
	port := getenv("PORT", "8080") // hosting platforms set PORT for you
	apiKey := os.Getenv("PULSE_API_KEY")
	allowedOrigin := getenv("ALLOWED_ORIGIN", "*")
 
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
 
	s := &Server{db: pool, apiKey: apiKey, allowedOrigin: allowedOrigin}
 
	mux := http.NewServeMux()
	mux.HandleFunc("POST /metrics", s.handleIngest)
	mux.HandleFunc("GET /metrics", s.handleQuery)
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
 
	for _, m := range metrics {
		if m.Host == "" || m.Name == "" {
			http.Error(w, "host and name are required", http.StatusBadRequest)
			return
		}
	}
 
	// Write the whole batch in one round trip using Postgres COPY,
	// instead of one INSERT (and one transaction) per metric.
	rows := make([][]any, len(metrics))
	for i, m := range metrics {
		rows[i] = []any{m.Host, m.Name, m.Value, time.Unix(m.Timestamp, 0)}
	}
 
	_, err := s.db.CopyFrom(r.Context(),
		pgx.Identifier{"metrics"},
		[]string{"host", "name", "value", "ts"},
		pgx.CopyFromRows(rows))
	if err != nil {
		log.Printf("batch insert failed: %v", err)
		http.Error(w, "database error", http.StatusInternalServerError)
		return
	}
 
	w.WriteHeader(http.StatusAccepted)
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
 