package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strings"
)

// ServerID returns a random identifier generated on first start and kept in the database.
// Devices use it to confirm that a discovered LAN address is really this server.
func (s *Store) ServerID(ctx context.Context) (string, error) {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return "", err
	}
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'server_id'`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	b := make([]byte, 16)
	rand.Read(b)
	id = hex.EncodeToString(b)
	_, err = s.db.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES('server_id', ?)`, id)
	return id, err
}

// lanURLs lists the addresses at which devices on the local network can reach this server.
// DROIDBEAM_LAN_URL overrides auto-detection (needed in Docker or behind port mapping).
func lanURLs(cfg Config) []string {
	if cfg.LANURL != "" {
		var out []string
		for _, u := range strings.Split(cfg.LANURL, ",") {
			if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
				out = append(out, u)
			}
		}
		return out
	}
	host, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		if ip.IsPrivate() {
			return []string{"http://" + net.JoinHostPort(ip.String(), port)}
		}
		return nil // bound to loopback or a public IP only: not reachable from the LAN
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := []string{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || !ip.IsPrivate() {
				continue
			}
			out = append(out, "http://"+net.JoinHostPort(ip.String(), port))
		}
	}
	return out
}

func (s *Server) handleServerInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"server_id": s.serverID,
		"lan_urls":  lanURLs(s.cfg),
	})
}
