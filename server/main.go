package main
 
import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
 
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
	db     *pgxpool.Pool
	apiKey string
}
 
func main() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://pulse:pulse@localhost:5432/pulse"
	}
 
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
 
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("could not create db pool: %v", err)
	}
	defer pool.Close()
 
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("cannot reach database (is docker running?): %v", err)
	}
 
	// Agents must send this key to write metrics. Refuse to start without one
	// so the server is never accidentally deployed unlocked.
	apiKey := os.Getenv("PULSE_API_KEY")
	if apiKey == "" {
		log.Fatal("PULSE_API_KEY is not set")
	}
 
	s := &Server{db: pool, apiKey: apiKey}
 
	mux := http.NewServeMux()
	mux.HandleFunc("POST /metrics", s.requireKey(s.handleIngest))
	mux.HandleFunc("GET /metrics", s.handleQuery)
 
	// Hosts like Render pick the port and pass it in PORT.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("server listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
 
// requireKey rejects requests that don't carry "Authorization: Bearer <PULSE_API_KEY>".
func (s *Server) requireKey(next http.HandlerFunc) http.HandlerFunc {
	want := []byte("Bearer " + s.apiKey)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
 
// handleIngest receives metrics from agents and stores them.
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
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
 
	for _, m := range metrics {
		_, err := s.db.Exec(r.Context(),
			`INSERT INTO metrics (host, name, value, ts) VALUES ($1, $2, $3, $4)`,
			m.Host, m.Name, m.Value, time.Unix(m.Timestamp, 0))
		if err != nil {
			log.Printf("insert failed: %v", err)
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
	}
 
	w.WriteHeader(http.StatusAccepted)
}
 
// handleQuery returns stored metrics for one host and metric name over a time window.
// Example: GET /metrics?host=my-laptop&name=cpu.percent&since=1h
func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	// Allow the dashboard (running on a different port/domain) to call this endpoint.
	w.Header().Set("Access-Control-Allow-Origin", "*")
 
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