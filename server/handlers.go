package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	maxBodyChars   = 1600
	claimBatchSize = 10
	defaultWait    = 25 * time.Second
	maxWait        = 50 * time.Second
)

type Server struct {
	cfg      Config
	serverID string
	store    *Store
	notify   *notifier
	webhook  *Webhook
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "server_id": s.serverID})
	})
	mux.Handle("GET /api/v1/server-info", s.requireAPIKey(s.handleServerInfo))
	mux.Handle("GET /api/v1/stats", s.requireAPIKey(s.handleStats))

	// Web dashboard at "/" (static files; data comes from the API above).
	mux.Handle("GET /", dashboardHandler())
	mux.Handle("GET /api/v1/device/server-info", s.requireDevice(s.handleServerInfo))

	// Client API (X-API-Key).
	mux.Handle("POST /api/v1/messages", s.requireAPIKey(s.handleCreateMessage))
	mux.Handle("GET /api/v1/messages", s.requireAPIKey(s.handleListMessages))
	mux.Handle("GET /api/v1/messages/{id}", s.requireAPIKey(s.handleGetMessage))
	mux.Handle("GET /api/v1/inbox", s.requireAPIKey(s.handleListInbox))
	mux.Handle("GET /api/v1/devices", s.requireAPIKey(s.handleListDevices))
	mux.Handle("GET /api/v1/devices/{id}", s.requireAPIKey(s.handleGetDevice))
	mux.Handle("POST /api/v1/devices", s.requireAPIKey(s.handleRegisterDevice))

	// Device API (Authorization: Bearer <device token>).
	mux.Handle("GET /api/v1/device/outbox", s.requireDevice(s.handleOutbox))
	mux.Handle("POST /api/v1/device/messages/{id}/status", s.requireDevice(s.handleStatus))
	mux.Handle("POST /api/v1/device/inbox", s.requireDevice(s.handleIncoming))
	mux.Handle("POST /api/v1/device/heartbeat", s.requireDevice(s.handleHeartbeat))

	return logRequests(mux)
}

// ---- auth ----

type ctxKey int

const deviceIDKey ctxKey = 0

func (s *Server) requireAPIKey(next http.HandlerFunc) http.Handler {
	want := []byte(s.cfg.APIKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("X-API-Key"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing X-API-Key")
			return
		}
		next(w, r)
	})
}

func (s *Server) requireDevice(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "missing device token")
			return
		}
		id, err := s.store.DeviceByTokenHash(r.Context(), hashToken(token))
		if errors.Is(err, ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "unknown device token")
			return
		} else if err != nil {
			internalError(w, err)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), deviceIDKey, id)))
	})
}

func deviceID(r *http.Request) int64 { return r.Context().Value(deviceIDKey).(int64) }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- client handlers ----

func (s *Server) handleCreateMessage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To       string `json:"to"`
		Body     string `json:"body"`
		DeviceID *int64 `json:"device_id"`
		SIM      *int   `json:"sim"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.To = strings.TrimSpace(req.To)
	if !validPhone(req.To) {
		writeError(w, http.StatusBadRequest, "to must be a phone number (digits, optional leading +)")
		return
	}
	if req.Body == "" || utf8.RuneCountInString(req.Body) > maxBodyChars {
		writeError(w, http.StatusBadRequest, "body must be 1-1600 characters")
		return
	}
	if req.SIM != nil && (*req.SIM < 1 || *req.SIM > 4) {
		writeError(w, http.StatusBadRequest, "sim must be a SIM slot number (1 or 2)")
		return
	}
	if req.DeviceID != nil {
		ok, err := s.store.DeviceExists(r.Context(), *req.DeviceID)
		if err != nil {
			internalError(w, err)
			return
		}
		if !ok {
			writeError(w, http.StatusBadRequest, "device_id does not exist")
			return
		}
	}
	m, err := s.store.CreateMessage(r.Context(), req.To, req.Body, req.DeviceID, req.SIM)
	if err != nil {
		internalError(w, err)
		return
	}
	s.notify.broadcast()
	writeJSON(w, http.StatusAccepted, m)
}

func (s *Server) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	m, err := s.store.GetMessage(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found")
		return
	} else if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	switch status {
	case "", StatusQueued, StatusSending, StatusSent, StatusDelivered, StatusFailed:
	default:
		writeError(w, http.StatusBadRequest, "invalid status filter")
		return
	}
	limit, before, ok := pageParams(w, r)
	if !ok {
		return
	}
	msgs, err := s.store.ListMessages(r.Context(), status, searchParam(r), before, limit)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": msgs})
}

func (s *Server) handleListInbox(w http.ResponseWriter, r *http.Request) {
	limit, before, ok := pageParams(w, r)
	if !ok {
		return
	}
	msgs, err := s.store.ListInbox(r.Context(), searchParam(r), before, limit)
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": msgs})
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.store.ListDevices(r.Context())
	if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": devs})
}

func (s *Server) handleGetDevice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	d, err := s.store.GetDevice(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	} else if err != nil {
		internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) handleRegisterDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len(req.Name) > 100 {
		writeError(w, http.StatusBadRequest, "name must be 1-100 characters")
		return
	}
	token := newToken()
	d, err := s.store.CreateDevice(r.Context(), req.Name, hashToken(token))
	if err != nil {
		internalError(w, err)
		return
	}
	// The token is only ever returned here; the server stores its hash.
	writeJSON(w, http.StatusCreated, map[string]any{"device": d, "token": token})
}

// ---- device handlers ----

// handleOutbox long-polls for queued messages and claims them for this device.
func (s *Server) handleOutbox(w http.ResponseWriter, r *http.Request) {
	dev := deviceID(r)
	wait := defaultWait
	if v := r.URL.Query().Get("wait"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 0 {
			writeError(w, http.StatusBadRequest, "wait must be a non-negative number of seconds")
			return
		}
		wait = min(time.Duration(secs)*time.Second, maxWait)
	}
	if !s.touch(w, r, dev) {
		return
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		changed := s.notify.wait() // grab before claiming so no broadcast is missed
		msgs, err := s.store.ClaimMessages(r.Context(), dev, claimBatchSize)
		if err != nil {
			if r.Context().Err() == nil {
				internalError(w, err)
			}
			return
		}
		if len(msgs) > 0 {
			for i := range msgs {
				s.webhook.Send("message.status", msgs[i])
			}
			writeJSON(w, http.StatusOK, map[string]any{"data": msgs})
			return
		}
		select {
		case <-changed:
		case <-timer.C:
			writeJSON(w, http.StatusOK, map[string]any{"data": []Message{}})
			return
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		SIM    *int   `json:"sim"` // slot used to send
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.Status {
	case StatusSent, StatusDelivered:
	case StatusFailed:
		if req.Error == "" {
			req.Error = "unknown error"
		}
		if len(req.Error) > 500 {
			req.Error = req.Error[:500]
		}
	default:
		writeError(w, http.StatusBadRequest, "status must be sent, delivered or failed")
		return
	}
	m, changed, err := s.store.UpdateStatus(r.Context(), deviceID(r), id, req.Status, req.Error, req.SIM)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, "message not found for this device")
		return
	} else if err != nil {
		internalError(w, err)
		return
	}
	if changed {
		s.webhook.Send("message.status", m)
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleIncoming(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UID        string `json:"uid"`
		From       string `json:"from"`
		Body       string `json:"body"`
		ReceivedAt int64  `json:"received_at"` // unix milliseconds
		SIM        *int   `json:"sim"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.UID == "" || len(req.UID) > 100 || req.From == "" || len(req.From) > 100 {
		writeError(w, http.StatusBadRequest, "uid and from are required")
		return
	}
	received := time.UnixMilli(req.ReceivedAt)
	if req.ReceivedAt <= 0 {
		received = time.Now()
	}
	m, inserted, err := s.store.AddInbox(r.Context(), deviceID(r), req.UID, req.From, req.Body, received, req.SIM)
	if err != nil {
		internalError(w, err)
		return
	}
	if inserted {
		s.webhook.Send("sms.received", m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": m, "duplicate": !inserted})
}

// handleHeartbeat stores the device's info and battery state and fires battery webhooks.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Info    *DeviceInfo `json:"info"`
		Battery *Battery    `json:"battery"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if b := req.Battery; b != nil {
		if b.Level < 0 || b.Level > 100 {
			writeError(w, http.StatusBadRequest, "battery.level must be 0-100")
			return
		}
		if b.Plugged == "" {
			b.Plugged = "none"
		}
	}
	dev := deviceID(r)
	if !s.touch(w, r, dev) {
		return
	}
	d, prev, err := s.store.UpdateDeviceState(r.Context(), dev, req.Info, req.Battery)
	if err != nil {
		internalError(w, err)
		return
	}
	if req.Battery != nil {
		for _, ev := range batteryEvents(prev, d.Battery, s.cfg.BatteryThresholds) {
			payload := map[string]any{"device": d, "battery": d.Battery}
			for k, v := range ev.extra {
				payload[k] = v
			}
			s.webhook.Send(ev.name, payload)
		}
	}
	writeJSON(w, http.StatusOK, d)
}

// touch marks the device as seen and fires device.online when it reconnects.
func (s *Server) touch(w http.ResponseWriter, r *http.Request, id int64) bool {
	cameOnline, err := s.store.TouchDevice(r.Context(), id)
	if err != nil {
		internalError(w, err)
		return false
	}
	if cameOnline {
		if d, err := s.store.GetDevice(r.Context(), id); err == nil {
			s.webhook.Send("device.online", map[string]any{"device": d})
		}
	}
	return true
}

type batteryEvent struct {
	name  string
	extra map[string]any
}

// batteryEvents compares two battery reports and returns the webhook events to fire.
// No events fire for the very first report (prev == nil) to avoid noise on registration.
func batteryEvents(prev, cur *Battery, thresholds []int) []batteryEvent {
	if prev == nil || cur == nil {
		return nil
	}
	var out []batteryEvent
	for _, t := range thresholds {
		switch {
		case prev.Level > t && cur.Level <= t:
			out = append(out, batteryEvent{"battery.threshold", map[string]any{"threshold": t, "direction": "down"}})
		case prev.Level < t && cur.Level >= t:
			out = append(out, batteryEvent{"battery.threshold", map[string]any{"threshold": t, "direction": "up"}})
		}
	}
	if !prev.full() && cur.full() {
		out = append(out, batteryEvent{name: "battery.full"})
	}
	if !prev.pluggedIn() && cur.pluggedIn() {
		out = append(out, batteryEvent{name: "battery.charger_connected"})
	}
	if prev.pluggedIn() && !cur.pluggedIn() {
		out = append(out, batteryEvent{name: "battery.charger_disconnected"})
	}
	return out
}

// ---- helpers ----

func validPhone(s string) bool {
	digits := strings.TrimPrefix(s, "+")
	if len(digits) < 3 || len(digits) > 20 {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// searchParam is the optional "q" filter of list endpoints (max 100 chars).
func searchParam(r *http.Request) string {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(q)) > 100 {
		q = string([]rune(q)[:100])
	}
	return q
}

func pageParams(w http.ResponseWriter, r *http.Request) (limit int, before int64, ok bool) {
	q := r.URL.Query()
	limit = 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be 1-200")
			return 0, 0, false
		}
		limit = n
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "before must be a message id")
			return 0, 0, false
		}
		before = n
	}
	return limit, before, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func internalError(w http.ResponseWriter, err error) {
	log.Printf("internal error: %v", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, rec.code, time.Since(start).Round(time.Millisecond))
	})
}

// notifier wakes long-polling devices when new messages are queued.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotifier() *notifier { return &notifier{ch: make(chan struct{})} }

func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	close(n.ch)
	n.ch = make(chan struct{})
}
