package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestBatteryEvents(t *testing.T) {
	th := []int{20, 80}
	bat := func(level int, status, plugged string) *Battery {
		return &Battery{Level: level, Status: status, Plugged: plugged}
	}
	names := func(evs []batteryEvent) []string {
		out := []string{}
		for _, e := range evs {
			name := e.name
			if d, ok := e.extra["direction"]; ok {
				name += ":" + d.(string)
			}
			out = append(out, name)
		}
		return out
	}
	cases := []struct {
		name      string
		prev, cur *Battery
		want      []string
	}{
		{"first report is silent", nil, bat(10, "discharging", "none"), []string{}},
		{"drop below 20", bat(21, "discharging", "none"), bat(20, "discharging", "none"), []string{"battery.threshold:down"}},
		{"stays low", bat(19, "discharging", "none"), bat(18, "discharging", "none"), []string{}},
		{"rise to 80", bat(79, "charging", "ac"), bat(80, "charging", "ac"), []string{"battery.threshold:up"}},
		{"full", bat(99, "charging", "ac"), bat(100, "full", "ac"), []string{"battery.full"}},
		{"plug in", bat(50, "discharging", "none"), bat(50, "charging", "usb"), []string{"battery.charger_connected"}},
		{"unplug", bat(50, "charging", "usb"), bat(50, "discharging", "none"), []string{"battery.charger_disconnected"}},
		{"big jump crosses both", bat(10, "charging", "ac"), bat(90, "charging", "ac"), []string{"battery.threshold:up", "battery.threshold:up"}},
	}
	for _, c := range cases {
		got := names(batteryEvents(c.prev, c.cur, th))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

type hookRecorder struct {
	mu     sync.Mutex
	events []string
}

func (h *hookRecorder) has(name string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, e := range h.events {
		if e == name {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestHeartbeatAndDeviceAPI(t *testing.T) {
	rec := &hookRecorder{}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Event string }
		json.NewDecoder(r.Body).Decode(&body)
		rec.mu.Lock()
		rec.events = append(rec.events, body.Event)
		rec.mu.Unlock()
	}))
	defer hook.Close()

	store, err := OpenStore(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		cfg:     Config{APIKey: testKey, SendTimeout: time.Minute, BatteryThresholds: []int{20, 80}},
		store:   store,
		notify:  newNotifier(),
		webhook: newWebhook(hook.URL, "secret"),
	}
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	defer store.Close()
	e := &testEnv{t: t, ts: ts, store: store}
	id, token := e.registerDevice("Pixel")

	beat := func(level int, status, plugged string) Device {
		var d Device
		code := e.do("POST", "/api/v1/device/heartbeat", token, map[string]any{
			"info":    map[string]any{"manufacturer": "Google", "model": "Pixel 8", "android_version": "15", "sdk": 35, "connection": "primary"},
			"battery": map[string]any{"level": level, "status": status, "plugged": plugged, "temperature": 31.5},
		}, &d)
		if code != 200 {
			t.Fatalf("heartbeat: %d", code)
		}
		return d
	}

	d := beat(25, "discharging", "none")
	if !d.Online || d.Info == nil || d.Info.Model != "Pixel 8" || d.Battery == nil || d.Battery.Level != 25 {
		t.Fatalf("device after heartbeat: %+v", d)
	}
	if !waitFor(t, func() bool { return rec.has("device.online") }) {
		t.Fatal("no device.online webhook")
	}
	beat(19, "discharging", "none")
	if !waitFor(t, func() bool { return rec.has("battery.threshold") }) {
		t.Fatal("no battery.threshold webhook")
	}
	beat(19, "charging", "ac")
	if !waitFor(t, func() bool { return rec.has("battery.charger_connected") }) {
		t.Fatal("no charger_connected webhook")
	}

	var got Device
	if code := e.do("GET", "/api/v1/devices/"+itoa(id), "key", nil, &got); code != 200 {
		t.Fatalf("get device: %d", code)
	}
	if got.Battery == nil || got.Battery.Level != 19 || got.Battery.Plugged != "ac" || *got.Battery.Temperature != 31.5 {
		t.Fatalf("get device battery: %+v", got.Battery)
	}
	if code := e.do("GET", "/api/v1/devices/999", "key", nil, nil); code != 404 {
		t.Fatalf("missing device: %d", code)
	}

	// Silence → offline webhook; next contact → online again.
	offline, err := store.MarkOffline(context.Background(), time.Now().Add(time.Second))
	if err != nil || len(offline) != 1 {
		t.Fatalf("MarkOffline = %v, %v", offline, err)
	}
	e.do("GET", "/api/v1/device/outbox?wait=0", token, nil, nil)
	e.do("GET", "/api/v1/devices/"+itoa(id), "key", nil, &got)
	if !got.Online {
		t.Fatal("device not back online after polling")
	}
}
