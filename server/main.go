package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	Addr          string
	DBPath        string
	APIKey        string
	WebhookURL    string
	WebhookSecret string
	// Comma-separated LAN address(es) advertised to devices; auto-detected when empty.
	LANURL string
	// Messages claimed by a device but not reported within this window are marked failed.
	SendTimeout time.Duration
	// Battery levels (%) that fire battery.threshold webhooks when crossed in either direction.
	BatteryThresholds []int
	// A device that has not contacted the server for this long is reported offline.
	OfflineAfter time.Duration
}

func loadConfig() Config {
	cfg := Config{
		Addr:          getenv("DROIDBEAM_ADDR", ":8080"),
		DBPath:        getenv("DROIDBEAM_DB", "droidbeam.db"),
		APIKey:        os.Getenv("DROIDBEAM_API_KEY"),
		WebhookURL:    os.Getenv("DROIDBEAM_WEBHOOK_URL"),
		WebhookSecret: os.Getenv("DROIDBEAM_WEBHOOK_SECRET"),
		LANURL:        os.Getenv("DROIDBEAM_LAN_URL"),
		SendTimeout:   10 * time.Minute,
		OfflineAfter:  2 * time.Minute,
	}
	thresholds := getenv("DROIDBEAM_BATTERY_THRESHOLDS", "20,80")
	for _, part := range strings.Split(thresholds, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > 100 {
			log.Fatalf("DROIDBEAM_BATTERY_THRESHOLDS: invalid level %q", part)
		}
		cfg.BatteryThresholds = append(cfg.BatteryThresholds, n)
	}
	if v := os.Getenv("DROIDBEAM_OFFLINE_AFTER"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < maxWait+10*time.Second {
			log.Fatalf("DROIDBEAM_OFFLINE_AFTER: must be a duration of at least %s", maxWait+10*time.Second)
		}
		cfg.OfflineAfter = d
	}
	if v := os.Getenv("DROIDBEAM_SEND_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			log.Fatalf("DROIDBEAM_SEND_TIMEOUT: invalid duration %q", v)
		}
		cfg.SendTimeout = d
	}
	if len(cfg.APIKey) < 16 {
		log.Fatal("DROIDBEAM_API_KEY must be set to a secret of at least 16 characters (e.g. `openssl rand -hex 24`)")
	}
	return cfg
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// "healthcheck" lets the Docker HEALTHCHECK probe the server without curl in the image.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "version", "--version", "-v":
			fmt.Println("droidbeam-server", version)
			return
		}
	}
	cfg := loadConfig()
	store, err := OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer store.Close()

	serverID, err := store.ServerID(context.Background())
	if err != nil {
		log.Fatalf("server id: %v", err)
	}
	srv := &Server{
		serverID: serverID,
		cfg:      cfg,
		store:    store,
		notify:   newNotifier(),
		webhook:  newWebhook(cfg.WebhookURL, cfg.WebhookSecret),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.sweepStale(ctx)

	httpServer := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      maxWait + 15*time.Second, // must outlast the outbox long-poll
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("DroidBeam server %s listening on %s (db: %s, id: %s)", version, cfg.Addr, cfg.DBPath, serverID)
	log.Printf("LAN address(es) advertised to devices: %v", lanURLs(cfg))
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// sweepStale fails messages a device claimed but never reported on, and flags silent devices offline.
func (s *Server) sweepStale(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			msgs, err := s.store.FailStale(ctx, time.Now().Add(-s.cfg.SendTimeout))
			if err != nil {
				log.Printf("sweep: %v", err)
				continue
			}
			for i := range msgs {
				s.webhook.Send("message.status", msgs[i])
			}
			devs, err := s.store.MarkOffline(ctx, time.Now().Add(-s.cfg.OfflineAfter))
			if err != nil {
				log.Printf("sweep: %v", err)
				continue
			}
			for i := range devs {
				s.webhook.Send("device.offline", map[string]any{"device": devs[i]})
			}
		}
	}
}

// healthcheck calls /healthz on the local server and returns a process exit code.
func healthcheck() int {
	_, port, err := net.SplitHostPort(getenv("DROIDBEAM_ADDR", ":8080"))
	if err != nil {
		return 1
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
