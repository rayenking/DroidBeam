package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

const testKey = "test-api-key-0123456789"

type testEnv struct {
	t     *testing.T
	ts    *httptest.Server
	store *Store
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		cfg:     Config{APIKey: testKey, SendTimeout: time.Minute},
		store:   store,
		notify:  newNotifier(),
		webhook: newWebhook("", ""),
	}
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(func() { ts.Close(); store.Close() })
	return &testEnv{t: t, ts: ts, store: store}
}

// do sends a request; auth is either "key" (client API key), "" (none) or a device token.
func (e *testEnv) do(method, path, auth string, body any, out any) int {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	switch auth {
	case "":
	case "key":
		req.Header.Set("X-API-Key", testKey)
	default:
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("%s %s: decode: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (e *testEnv) registerDevice(name string) (int64, string) {
	e.t.Helper()
	var out struct {
		Device Device `json:"device"`
		Token  string `json:"token"`
	}
	if code := e.do("POST", "/api/v1/devices", "key", map[string]string{"name": name}, &out); code != 201 {
		e.t.Fatalf("register: %d", code)
	}
	return out.Device.ID, out.Token
}

func TestAuth(t *testing.T) {
	e := newTestEnv(t)
	if code := e.do("GET", "/api/v1/messages", "", nil, nil); code != 401 {
		t.Errorf("no key: got %d", code)
	}
	if code := e.do("GET", "/api/v1/device/outbox?wait=0", "bogus", nil, nil); code != 401 {
		t.Errorf("bad device token: got %d", code)
	}
	if code := e.do("GET", "/api/v1/messages", "key", nil, nil); code != 200 {
		t.Errorf("valid key: got %d", code)
	}
}

func TestSendFlow(t *testing.T) {
	e := newTestEnv(t)
	_, token := e.registerDevice("Pixel")

	var msg Message
	if code := e.do("POST", "/api/v1/messages", "key", map[string]string{"to": "+6281234567890", "body": "halo"}, &msg); code != 202 {
		t.Fatalf("create: %d", code)
	}
	if msg.Status != StatusQueued {
		t.Fatalf("status = %s", msg.Status)
	}

	var outbox struct{ Data []Message }
	e.do("GET", "/api/v1/device/outbox?wait=0", token, nil, &outbox)
	if len(outbox.Data) != 1 || outbox.Data[0].ID != msg.ID || outbox.Data[0].Status != StatusSending {
		t.Fatalf("outbox = %+v", outbox.Data)
	}
	// Claimed messages must not be handed out again.
	e.do("GET", "/api/v1/device/outbox?wait=0", token, nil, &outbox)
	if len(outbox.Data) != 0 {
		t.Fatalf("message claimed twice: %+v", outbox.Data)
	}

	path := "/api/v1/device/messages/" + itoa(msg.ID) + "/status"
	e.do("POST", path, token, map[string]string{"status": "delivered"}, &msg)
	if msg.Status != StatusDelivered || msg.DeliveredAt == nil || msg.SentAt == nil {
		t.Fatalf("after delivered: %+v", msg)
	}
	// A late "sent" report must not downgrade a delivered message.
	e.do("POST", path, token, map[string]string{"status": "sent"}, &msg)
	if msg.Status != StatusDelivered {
		t.Fatalf("downgraded to %s", msg.Status)
	}

	e.do("GET", "/api/v1/messages/"+itoa(msg.ID), "key", nil, &msg)
	if msg.Status != StatusDelivered {
		t.Fatalf("get: %s", msg.Status)
	}
}

func TestStatusFromOtherDeviceRejected(t *testing.T) {
	e := newTestEnv(t)
	_, tokA := e.registerDevice("A")
	_, tokB := e.registerDevice("B")
	var msg Message
	e.do("POST", "/api/v1/messages", "key", map[string]string{"to": "08123", "body": "x"}, &msg)
	var outbox struct{ Data []Message }
	e.do("GET", "/api/v1/device/outbox?wait=0", tokA, nil, &outbox)
	if code := e.do("POST", "/api/v1/device/messages/"+itoa(msg.ID)+"/status", tokB,
		map[string]string{"status": "sent"}, nil); code != 404 {
		t.Fatalf("other device status: %d", code)
	}
}

func TestTargetedDevice(t *testing.T) {
	e := newTestEnv(t)
	idA, tokA := e.registerDevice("A")
	_, tokB := e.registerDevice("B")
	e.do("POST", "/api/v1/messages", "key", map[string]any{"to": "08123", "body": "x", "device_id": idA}, nil)

	var outbox struct{ Data []Message }
	e.do("GET", "/api/v1/device/outbox?wait=0", tokB, nil, &outbox)
	if len(outbox.Data) != 0 {
		t.Fatal("device B received a message targeted at A")
	}
	e.do("GET", "/api/v1/device/outbox?wait=0", tokA, nil, &outbox)
	if len(outbox.Data) != 1 {
		t.Fatal("device A did not receive its message")
	}
}

func TestLongPollWakesOnNewMessage(t *testing.T) {
	e := newTestEnv(t)
	_, token := e.registerDevice("Pixel")

	got := make(chan int, 1)
	go func() {
		var outbox struct{ Data []Message }
		e.do("GET", "/api/v1/device/outbox?wait=10", token, nil, &outbox)
		got <- len(outbox.Data)
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	e.do("POST", "/api/v1/messages", "key", map[string]string{"to": "08123", "body": "x"}, nil)
	select {
	case n := <-got:
		if n != 1 {
			t.Fatalf("got %d messages", n)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatal("long-poll was not woken promptly")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("long-poll never returned")
	}
}

func TestIncomingDedup(t *testing.T) {
	e := newTestEnv(t)
	_, token := e.registerDevice("Pixel")
	sms := map[string]any{"uid": "abc", "from": "+628111", "body": "OTP 1234", "received_at": time.Now().UnixMilli()}

	var res struct {
		Duplicate bool `json:"duplicate"`
	}
	e.do("POST", "/api/v1/device/inbox", token, sms, &res)
	if res.Duplicate {
		t.Fatal("first upload flagged duplicate")
	}
	e.do("POST", "/api/v1/device/inbox", token, sms, &res)
	if !res.Duplicate {
		t.Fatal("retry not flagged duplicate")
	}
	var inbox struct{ Data []InboxMessage }
	e.do("GET", "/api/v1/inbox", "key", nil, &inbox)
	if len(inbox.Data) != 1 || inbox.Data[0].Body != "OTP 1234" {
		t.Fatalf("inbox = %+v", inbox.Data)
	}
}

func TestValidation(t *testing.T) {
	e := newTestEnv(t)
	cases := []map[string]any{
		{"to": "abc", "body": "x"},
		{"to": "08123", "body": ""},
		{"to": "08123", "body": "x", "device_id": 999},
	}
	for _, c := range cases {
		if code := e.do("POST", "/api/v1/messages", "key", c, nil); code != 400 {
			t.Errorf("%v: got %d, want 400", c, code)
		}
	}
}

func TestFailStale(t *testing.T) {
	e := newTestEnv(t)
	_, token := e.registerDevice("Pixel")
	var msg Message
	e.do("POST", "/api/v1/messages", "key", map[string]string{"to": "08123", "body": "x"}, &msg)
	e.do("GET", "/api/v1/device/outbox?wait=0", token, nil, nil)

	failed, err := e.store.FailStale(context.Background(), time.Now().Add(time.Second))
	if err != nil || len(failed) != 1 || failed[0].Status != StatusFailed {
		t.Fatalf("FailStale = %+v, %v", failed, err)
	}
	// The device may still report success afterwards (e.g. it was offline).
	e.do("POST", "/api/v1/device/messages/"+itoa(msg.ID)+"/status", token, map[string]string{"status": "sent"}, &msg)
	if msg.Status != StatusSent || msg.Error != nil {
		t.Fatalf("recovered = %+v", msg)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestServerInfo(t *testing.T) {
	e := newTestEnv(t)
	_, token := e.registerDevice("Pixel")
	var info struct {
		ServerID string   `json:"server_id"`
		LANURLs  []string `json:"lan_urls"`
	}
	if code := e.do("GET", "/api/v1/device/server-info", token, nil, &info); code != 200 {
		t.Fatalf("server-info: %d", code)
	}
	id1, _ := e.store.ServerID(context.Background())
	id2, _ := e.store.ServerID(context.Background())
	if id1 == "" || id1 != id2 {
		t.Fatalf("server id not stable: %q %q", id1, id2)
	}
	got := lanURLs(Config{LANURL: "http://192.168.1.5:8080/, http://10.0.0.2:8080"})
	if len(got) != 2 || got[0] != "http://192.168.1.5:8080" {
		t.Fatalf("override: %v", got)
	}
	if got := lanURLs(Config{Addr: "127.0.0.1:8080"}); len(got) != 0 {
		t.Fatalf("loopback bind should advertise nothing: %v", got)
	}
}

func TestSIMSelection(t *testing.T) {
	e := newTestEnv(t)
	_, token := e.registerDevice("Redmi")
	var msg Message
	e.do("POST", "/api/v1/messages", "key", map[string]any{"to": "08123", "body": "x", "sim": 2}, &msg)
	if msg.SIM == nil || *msg.SIM != 2 {
		t.Fatalf("sim not stored: %+v", msg)
	}
	if code := e.do("POST", "/api/v1/messages", "key", map[string]any{"to": "08123", "body": "x", "sim": 9}, nil); code != 400 {
		t.Fatalf("invalid sim accepted: %d", code)
	}
	var outbox struct{ Data []Message }
	e.do("GET", "/api/v1/device/outbox?wait=0", token, nil, &outbox)
	if len(outbox.Data) != 1 || outbox.Data[0].SIM == nil || *outbox.Data[0].SIM != 2 {
		t.Fatalf("outbox sim: %+v", outbox.Data)
	}
	e.do("POST", "/api/v1/device/messages/"+itoa(msg.ID)+"/status", token, map[string]any{"status": "sent", "sim": 2}, &msg)
	if msg.SIMUsed == nil || *msg.SIMUsed != 2 {
		t.Fatalf("sim_used: %+v", msg)
	}
}

func TestMigrationAddsColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// Simulate a database from the first version (no sim / device state columns).
	old, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE messages`, `DROP TABLE inbox`, `DROP TABLE devices`,
		`CREATE TABLE devices (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, created_at INTEGER NOT NULL, last_seen_at INTEGER)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, recipient TEXT NOT NULL, body TEXT NOT NULL, status TEXT NOT NULL, device_id INTEGER, error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL, claimed_at INTEGER, sent_at INTEGER, delivered_at INTEGER)`,
		`CREATE TABLE inbox (id INTEGER PRIMARY KEY AUTOINCREMENT, device_id INTEGER NOT NULL, uid TEXT NOT NULL, sender TEXT NOT NULL, body TEXT NOT NULL, received_at INTEGER NOT NULL, created_at INTEGER NOT NULL, UNIQUE(device_id, uid))`,
	} {
		if _, err := old.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()
	st, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	sim := 1
	if _, err := st.CreateMessage(context.Background(), "08123", "x", nil, &sim); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
}

func TestDashboardAndStats(t *testing.T) {
	e := newTestEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("DroidBeam")) {
		t.Fatalf("dashboard: %d", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); csp == "" {
		t.Fatal("dashboard served without CSP")
	}
	for _, f := range []string{"/app.js", "/style.css"} {
		r, err := http.Get(e.ts.URL + f)
		if err != nil || r.StatusCode != 200 {
			t.Fatalf("%s: %v %v", f, err, r.StatusCode)
		}
		r.Body.Close()
	}

	e.do("POST", "/api/v1/messages", "key", map[string]string{"to": "08123", "body": "x"}, nil)
	var st Stats
	if code := e.do("GET", "/api/v1/stats", "key", nil, &st); code != 200 {
		t.Fatalf("stats: %d", code)
	}
	if st.Today.Messages["queued"] != 1 || st.Messages["queued"] != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if code := e.do("GET", "/api/v1/stats", "", nil, nil); code != 401 {
		t.Fatalf("stats without key: %d", code)
	}
}
