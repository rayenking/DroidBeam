package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Message statuses.
const (
	StatusQueued    = "queued"
	StatusSending   = "sending"
	StatusSent      = "sent"
	StatusDelivered = "delivered"
	StatusFailed    = "failed"
)

var ErrNotFound = errors.New("not found")

const schema = `
CREATE TABLE IF NOT EXISTS devices (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT    NOT NULL,
  token_hash   TEXT    NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER,
  online       INTEGER NOT NULL DEFAULT 0,
  info_json    TEXT,
  battery_json TEXT
);
CREATE TABLE IF NOT EXISTS messages (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  recipient    TEXT    NOT NULL,
  body         TEXT    NOT NULL,
  status       TEXT    NOT NULL,
  device_id    INTEGER REFERENCES devices(id),
  error        TEXT,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL,
  claimed_at   INTEGER,
  sent_at      INTEGER,
  delivered_at INTEGER,
  sim          INTEGER,
  sim_used     INTEGER
);
CREATE INDEX IF NOT EXISTS idx_messages_status ON messages(status, id);
CREATE TABLE IF NOT EXISTS inbox (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id   INTEGER NOT NULL REFERENCES devices(id),
  uid         TEXT    NOT NULL,
  sender      TEXT    NOT NULL,
  body        TEXT    NOT NULL,
  received_at INTEGER NOT NULL,
  created_at  INTEGER NOT NULL,
  sim         INTEGER,
  UNIQUE(device_id, uid)
);
`

type Device struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	Online     bool        `json:"online"`
	CreatedAt  time.Time   `json:"created_at"`
	LastSeenAt *time.Time  `json:"last_seen_at"`
	Info       *DeviceInfo `json:"info"`
	Battery    *Battery    `json:"battery"`
}

// DeviceInfo describes the phone, as reported by the app.
type DeviceInfo struct {
	Manufacturer   string    `json:"manufacturer"`
	Model          string    `json:"model"`
	AndroidVersion string    `json:"android_version"`
	SDK            int       `json:"sdk"`
	AppVersion     string    `json:"app_version"`
	Operator       string    `json:"operator"`   // SIM carrier name
	Network        string    `json:"network"`    // wifi | cellular | ethernet | none
	Connection     string    `json:"connection"` // which server address the app is using: primary | fallback
	LocalIP        string    `json:"local_ip"`   // the phone's own IP on its current network
	SIMs           []SIMInfo `json:"sims"`
}

// SIMInfo describes one active SIM card in the phone.
type SIMInfo struct {
	Slot       int    `json:"slot"` // 1 = SIM 1, 2 = SIM 2
	Carrier    string `json:"carrier"`
	InService  bool   `json:"in_service"`
	DefaultSMS bool   `json:"default_sms"`
}

type Battery struct {
	Level       int       `json:"level"`       // 0-100
	Status      string    `json:"status"`      // charging | discharging | full | not_charging | unknown
	Plugged     string    `json:"plugged"`     // ac | usb | wireless | dock | none
	Temperature *float64  `json:"temperature"` // °C
	Health      string    `json:"health"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (b *Battery) pluggedIn() bool { return b.Plugged != "" && b.Plugged != "none" }

func (b *Battery) full() bool {
	return b.Status == "full" || (b.Level >= 100 && b.pluggedIn())
}

type Message struct {
	ID          int64      `json:"id"`
	To          string     `json:"to"`
	Body        string     `json:"body"`
	Status      string     `json:"status"`
	DeviceID    *int64     `json:"device_id"`
	Error       *string    `json:"error"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	SentAt      *time.Time `json:"sent_at"`
	DeliveredAt *time.Time `json:"delivered_at"`
	SIM         *int       `json:"sim"`      // requested SIM slot; null = phone decides
	SIMUsed     *int       `json:"sim_used"` // slot the phone actually sent through
}

type InboxMessage struct {
	ID         int64     `json:"id"`
	DeviceID   int64     `json:"device_id"`
	From       string    `json:"from"`
	Body       string    `json:"body"`
	ReceivedAt time.Time `json:"received_at"`
	CreatedAt  time.Time `json:"created_at"`
	SIM        *int      `json:"sim"` // slot the SMS arrived on, if known
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func OpenStore(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; serialising through one connection avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := addColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// addColumns upgrades databases created before a column existed.
func addColumns(db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE devices ADD COLUMN online INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE devices ADD COLUMN info_json TEXT`,
		`ALTER TABLE devices ADD COLUMN battery_json TEXT`,
		`ALTER TABLE messages ADD COLUMN sim INTEGER`,
		`ALTER TABLE messages ADD COLUMN sim_used INTEGER`,
		`ALTER TABLE inbox ADD COLUMN sim INTEGER`,
	} {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) nowMs() int64 { return s.now().UnixMilli() }

func msToTime(v int64) time.Time { return time.UnixMilli(v).UTC() }

func nullInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	n := int(v.Int64)
	return &n
}

func nullMsToTime(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := msToTime(v.Int64)
	return &t
}

// ---- devices ----

func (s *Store) CreateDevice(ctx context.Context, name, tokenHash string) (*Device, error) {
	now := s.nowMs()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO devices(name, token_hash, created_at) VALUES(?, ?, ?)`, name, tokenHash, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Device{ID: id, Name: name, CreatedAt: msToTime(now)}, nil
}

func (s *Store) DeviceByTokenHash(ctx context.Context, tokenHash string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM devices WHERE token_hash = ?`, tokenHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

const deviceCols = `id, name, online, created_at, last_seen_at, info_json, battery_json`

func scanDevice(sc scanner) (*Device, error) {
	var d Device
	var online int
	var created int64
	var seen sql.NullInt64
	var info, battery sql.NullString
	if err := sc.Scan(&d.ID, &d.Name, &online, &created, &seen, &info, &battery); err != nil {
		return nil, err
	}
	d.Online = online == 1
	d.CreatedAt = msToTime(created)
	d.LastSeenAt = nullMsToTime(seen)
	if info.Valid {
		d.Info = new(DeviceInfo)
		if err := json.Unmarshal([]byte(info.String), d.Info); err != nil {
			return nil, err
		}
	}
	if battery.Valid {
		d.Battery = new(Battery)
		if err := json.Unmarshal([]byte(battery.String), d.Battery); err != nil {
			return nil, err
		}
	}
	return &d, nil
}

// TouchDevice records that the device is alive. cameOnline is true if it was marked offline before.
func (s *Store) TouchDevice(ctx context.Context, id int64) (cameOnline bool, err error) {
	now := s.nowMs()
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET online = 1, last_seen_at = ? WHERE id = ? AND online = 0`, now, id)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	_, err = s.db.ExecContext(ctx, `UPDATE devices SET last_seen_at = ? WHERE id = ?`, now, id)
	return false, err
}

// MarkOffline flags devices not seen since cutoff as offline and returns them.
func (s *Store) MarkOffline(ctx context.Context, cutoff time.Time) ([]Device, error) {
	return s.queryDevices(ctx,
		`UPDATE devices SET online = 0
		 WHERE online = 1 AND (last_seen_at IS NULL OR last_seen_at < ?)
		 RETURNING `+deviceCols, cutoff.UnixMilli())
}

// UpdateDeviceState stores the latest info/battery report and returns the previous battery state.
func (s *Store) UpdateDeviceState(ctx context.Context, id int64, info *DeviceInfo, battery *Battery) (dev *Device, prev *Battery, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	before, err := scanDevice(tx.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	} else if err != nil {
		return nil, nil, err
	}
	if info != nil {
		b, _ := json.Marshal(info)
		if _, err := tx.ExecContext(ctx, `UPDATE devices SET info_json = ? WHERE id = ?`, string(b), id); err != nil {
			return nil, nil, err
		}
	}
	if battery != nil {
		battery.UpdatedAt = msToTime(s.nowMs())
		b, _ := json.Marshal(battery)
		if _, err := tx.ExecContext(ctx, `UPDATE devices SET battery_json = ? WHERE id = ?`, string(b), id); err != nil {
			return nil, nil, err
		}
	}
	dev, err = scanDevice(tx.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
	if err != nil {
		return nil, nil, err
	}
	return dev, before.Battery, tx.Commit()
}

func (s *Store) DeviceExists(ctx context.Context, id int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE id = ?`, id).Scan(&n)
	return n > 0, err
}

func (s *Store) GetDevice(ctx context.Context, id int64) (*Device, error) {
	d, err := scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	return s.queryDevices(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY id`)
}

func (s *Store) queryDevices(ctx context.Context, q string, args ...any) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// ---- messages ----

const messageCols = `id, recipient, body, status, device_id, error, created_at, updated_at, sent_at, delivered_at, sim, sim_used`

type scanner interface{ Scan(dest ...any) error }

func scanMessage(sc scanner) (*Message, error) {
	var m Message
	var dev sql.NullInt64
	var errMsg sql.NullString
	var created, updated int64
	var sent, delivered, sim, simUsed sql.NullInt64
	if err := sc.Scan(&m.ID, &m.To, &m.Body, &m.Status, &dev, &errMsg, &created, &updated, &sent, &delivered, &sim, &simUsed); err != nil {
		return nil, err
	}
	if dev.Valid {
		m.DeviceID = &dev.Int64
	}
	if errMsg.Valid {
		m.Error = &errMsg.String
	}
	m.CreatedAt = msToTime(created)
	m.UpdatedAt = msToTime(updated)
	m.SentAt = nullMsToTime(sent)
	m.DeliveredAt = nullMsToTime(delivered)
	m.SIM = nullInt(sim)
	m.SIMUsed = nullInt(simUsed)
	return &m, nil
}

func (s *Store) CreateMessage(ctx context.Context, to, body string, deviceID *int64, sim *int) (*Message, error) {
	now := s.nowMs()
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO messages(recipient, body, status, device_id, sim, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?) RETURNING `+messageCols,
		to, body, StatusQueued, deviceID, sim, now, now)
	return scanMessage(row)
}

func (s *Store) GetMessage(ctx context.Context, id int64) (*Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return m, err
}

// ListMessages returns newest first. beforeID = 0 means no cursor.
func (s *Store) ListMessages(ctx context.Context, status string, beforeID int64, limit int) ([]Message, error) {
	q := `SELECT ` + messageCols + ` FROM messages WHERE 1=1`
	args := []any{}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if beforeID > 0 {
		q += ` AND id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	return s.queryMessages(ctx, q, args...)
}

func (s *Store) queryMessages(ctx context.Context, q string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// ClaimMessages atomically moves up to limit queued messages to "sending" for the device.
func (s *Store) ClaimMessages(ctx context.Context, deviceID int64, limit int) ([]Message, error) {
	now := s.nowMs()
	msgs, err := s.queryMessages(ctx,
		`UPDATE messages SET status = ?, device_id = ?, claimed_at = ?, updated_at = ?
		 WHERE id IN (
		   SELECT id FROM messages
		   WHERE status = ? AND (device_id IS NULL OR device_id = ?)
		   ORDER BY id LIMIT ?)
		 RETURNING `+messageCols,
		StatusSending, deviceID, now, now, StatusQueued, deviceID, limit)
	if err != nil {
		return nil, err
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	return msgs, nil
}

// UpdateStatus applies a status report from the device that claimed the message.
// changed is false when the report did not alter anything (duplicate or out-of-order report).
func (s *Store) UpdateStatus(ctx context.Context, deviceID, msgID int64, status, errMsg string, simUsed *int) (m *Message, changed bool, err error) {
	now := s.nowMs()
	var res sql.Result
	switch status {
	case StatusSent:
		res, err = s.db.ExecContext(ctx,
			`UPDATE messages SET status = ?, sent_at = COALESCE(sent_at, ?), error = NULL, updated_at = ?,
			   sim_used = COALESCE(?, sim_used)
			 WHERE id = ? AND device_id = ? AND status IN (?, ?)`,
			StatusSent, now, now, simUsed, msgID, deviceID, StatusSending, StatusFailed)
	case StatusDelivered:
		res, err = s.db.ExecContext(ctx,
			`UPDATE messages SET status = ?, sent_at = COALESCE(sent_at, ?), delivered_at = ?, error = NULL, updated_at = ?,
			   sim_used = COALESCE(?, sim_used)
			 WHERE id = ? AND device_id = ? AND status IN (?, ?, ?)`,
			StatusDelivered, now, now, now, simUsed, msgID, deviceID, StatusSending, StatusSent, StatusFailed)
	case StatusFailed:
		res, err = s.db.ExecContext(ctx,
			`UPDATE messages SET status = ?, error = ?, updated_at = ?, sim_used = COALESCE(?, sim_used)
			 WHERE id = ? AND device_id = ? AND status IN (?, ?)`,
			StatusFailed, errMsg, now, simUsed, msgID, deviceID, StatusSending, StatusSent)
	default:
		return nil, false, fmt.Errorf("invalid status %q", status)
	}
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	m, err = scanMessage(s.db.QueryRowContext(ctx,
		`SELECT `+messageCols+` FROM messages WHERE id = ? AND device_id = ?`, msgID, deviceID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	return m, n > 0, err
}

// FailStale marks messages stuck in "sending" since before cutoff as failed.
func (s *Store) FailStale(ctx context.Context, cutoff time.Time) ([]Message, error) {
	now := s.nowMs()
	return s.queryMessages(ctx,
		`UPDATE messages SET status = ?, error = ?, updated_at = ?
		 WHERE status = ? AND claimed_at < ?
		 RETURNING `+messageCols,
		StatusFailed, "device did not report a result in time", now, StatusSending, cutoff.UnixMilli())
}

// ---- inbox ----

// AddInbox stores an incoming SMS. inserted is false if the device already uploaded this uid.
func (s *Store) AddInbox(ctx context.Context, deviceID int64, uid, from, body string, receivedAt time.Time, sim *int) (msg *InboxMessage, inserted bool, err error) {
	now := s.nowMs()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO inbox(device_id, uid, sender, body, received_at, created_at, sim)
		 VALUES(?, ?, ?, ?, ?, ?, ?) ON CONFLICT(device_id, uid) DO NOTHING`,
		deviceID, uid, from, body, receivedAt.UnixMilli(), now, sim)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	row := s.db.QueryRowContext(ctx,
		`SELECT id, device_id, sender, body, received_at, created_at, sim FROM inbox WHERE device_id = ? AND uid = ?`,
		deviceID, uid)
	msg, err = scanInbox(row)
	return msg, n > 0, err
}

func scanInbox(sc scanner) (*InboxMessage, error) {
	var m InboxMessage
	var received, created int64
	var sim sql.NullInt64
	if err := sc.Scan(&m.ID, &m.DeviceID, &m.From, &m.Body, &received, &created, &sim); err != nil {
		return nil, err
	}
	m.SIM = nullInt(sim)
	m.ReceivedAt = msToTime(received)
	m.CreatedAt = msToTime(created)
	return &m, nil
}

func (s *Store) ListInbox(ctx context.Context, beforeID int64, limit int) ([]InboxMessage, error) {
	q := `SELECT id, device_id, sender, body, received_at, created_at, sim FROM inbox`
	args := []any{}
	if beforeID > 0 {
		q += ` WHERE id < ?`
		args = append(args, beforeID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []InboxMessage{}
	for rows.Next() {
		m, err := scanInbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}
