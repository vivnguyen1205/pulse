package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Metric struct {
	Host      string  `json:"host"`
	Name      string  `json:"name"`
	Value     float64 `json:"value"`
	Timestamp int64   `json:"timestamp"`
}

type Server struct {
	db *pgxpool.Pool
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

	s := &Server{db: pool}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /metrics", s.handleIngest)

	log.Println("server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", mux))
}

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