package main

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"time"
)

//go:embed web
var webFS embed.FS

// dashboardHandler serves the embedded web dashboard. The page holds no secrets:
// it asks for the API key and calls the same REST API as any other client.
func dashboardHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

type Stats struct {
	Messages map[string]int `json:"messages"` // all time, by status
	Today    struct {
		Messages map[string]int `json:"messages"`
		Inbox    int            `json:"inbox"`
	} `json:"today"`
}

func (s *Store) Stats(ctx context.Context, since time.Time) (*Stats, error) {
	st := &Stats{Messages: map[string]int{}}
	st.Today.Messages = map[string]int{}
	count := func(dst map[string]int, q string, args ...any) error {
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var status string
			var n int
			if err := rows.Scan(&status, &n); err != nil {
				return err
			}
			dst[status] = n
		}
		return rows.Err()
	}
	if err := count(st.Messages, `SELECT status, COUNT(*) FROM messages GROUP BY status`); err != nil {
		return nil, err
	}
	if err := count(st.Today.Messages, `SELECT status, COUNT(*) FROM messages WHERE created_at >= ? GROUP BY status`, since.UnixMilli()); err != nil {
		return nil, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox WHERE created_at >= ?`, since.UnixMilli()).Scan(&st.Today.Inbox)
	return st, err
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	st, err := s.store.Stats(r.Context(), midnight)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}
