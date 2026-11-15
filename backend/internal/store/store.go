// Package store implements the SQLite persistence layer.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/aclab/restreamcamera/internal/model"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS sources (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  kind        TEXT NOT NULL,
  url_enc     TEXT NOT NULL DEFAULT '',
  transport   TEXT NOT NULL DEFAULT 'tcp',
  enabled     INTEGER NOT NULL DEFAULT 1,
  status      TEXT NOT NULL DEFAULT 'unknown',
  last_error  TEXT NOT NULL DEFAULT '',
  last_seen_at TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS cameras (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  slug         TEXT NOT NULL UNIQUE,
  source_id    TEXT NOT NULL,
  group_name   TEXT NOT NULL DEFAULT '',
  enabled      INTEGER NOT NULL DEFAULT 1,
  on_demand    INTEGER NOT NULL DEFAULT 1,
  max_readers  INTEGER NOT NULL DEFAULT 4,
  mode         TEXT NOT NULL DEFAULT 'proxy',
  read_user    TEXT NOT NULL DEFAULT '',
  pass_enc     TEXT NOT NULL DEFAULT '',
  ip_allowlist TEXT NOT NULL DEFAULT '',
  vendor       TEXT NOT NULL DEFAULT '',
  model        TEXT NOT NULL DEFAULT '',
  serial       TEXT NOT NULL DEFAULT '',
  firmware     TEXT NOT NULL DEFAULT '',
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cameras_source ON cameras(source_id);

CREATE TABLE IF NOT EXISTS runtime (
  camera_id      TEXT PRIMARY KEY,
  state          TEXT NOT NULL DEFAULT 'unknown',
  readers        INTEGER NOT NULL DEFAULT 0,
  inbound_bytes  INTEGER NOT NULL DEFAULT 0,
  outbound_bytes INTEGER NOT NULL DEFAULT 0,
  in_kbps        INTEGER NOT NULL DEFAULT 0,
  out_kbps       INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT NOT NULL DEFAULT '',
  updated_at     TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts       TEXT NOT NULL,
  level    TEXT NOT NULL,
  category TEXT NOT NULL,
  subject  TEXT NOT NULL DEFAULT '',
  message  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts DESC);

CREATE TABLE IF NOT EXISTS samples (
  ts            INTEGER PRIMARY KEY,
  readers       INTEGER NOT NULL,
  inbound_bytes INTEGER NOT NULL,
  outbound_bytes INTEGER NOT NULL,
  ready         INTEGER NOT NULL
);
`

// Open opens (and migrates) the database at dbPath.
func Open(dbPath string) (*Store, error) {
	if dir := filepath.Dir(dbPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	// A single connection keeps SQLite writes serialized and avoids
	// SQLITE_BUSY under the reconciler + API workload.
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ---------------------------------------------------------------- sources

// CreateSource inserts a source. urlEnc is the encrypted URL (may be empty).
func (s *Store) CreateSource(ctx context.Context, in model.Source, urlEnc string) (model.Source, error) {
	now := time.Now().UTC()
	in.ID = newID()
	in.CreatedAt, in.UpdatedAt = now, now
	if in.Kind == "" {
		in.Kind = "pull"
	}
	if in.Transport == "" {
		in.Transport = "tcp"
	}
	if in.Status == "" {
		in.Status = "unknown"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sources (id,name,kind,url_enc,transport,enabled,status,last_error,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?)`,
		in.ID, in.Name, in.Kind, urlEnc, in.Transport, boolInt(in.Enabled), in.Status,
		"", ts(now), ts(now))
	if err != nil {
		return model.Source{}, err
	}
	in.URLMasked = maskURL(in.URL)
	in.HasURL = urlEnc != ""
	in.Path = "src-" + shortID(in.ID)
	return in, nil
}

// UpdateSource updates mutable fields. urlEnc == nil keeps the stored URL.
func (s *Store) UpdateSource(ctx context.Context, id string, in model.Source, urlEnc *string) error {
	now := time.Now().UTC()
	if urlEnc != nil {
		_, err := s.db.ExecContext(ctx, `
			UPDATE sources SET name=?, kind=?, transport=?, enabled=?, url_enc=?, updated_at=? WHERE id=?`,
			in.Name, in.Kind, in.Transport, boolInt(in.Enabled), *urlEnc, ts(now), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE sources SET name=?, kind=?, transport=?, enabled=?, updated_at=? WHERE id=?`,
		in.Name, in.Kind, in.Transport, boolInt(in.Enabled), ts(now), id)
	return err
}

// DeleteSource removes a source and the cameras using it.
func (s *Store) DeleteSource(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM cameras WHERE source_id=?`, id); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runtime WHERE camera_id NOT IN (SELECT id FROM cameras)`); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM sources WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetSource returns a source by id, including its encrypted URL.
func (s *Store) GetSource(ctx context.Context, id string) (model.Source, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id,name,kind,url_enc,transport,enabled,status,last_error,created_at,updated_at
		FROM sources WHERE id=?`, id)
	var (
		out              model.Source
		urlEnc           string
		enabled          int
		created, updated string
	)
	if err := row.Scan(&out.ID, &out.Name, &out.Kind, &urlEnc, &out.Transport, &enabled,
		&out.Status, &out.LastError, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Source{}, ErrNotFound
		}
		return model.Source{}, err
	}
	out.URLEnc = urlEnc
	out.HasURL = urlEnc != ""
	out.URLMasked = "••••••"
	if urlEnc == "" {
		out.URLMasked = ""
	}
	out.Enabled = enabled == 1
	out.CreatedAt, out.UpdatedAt = parseTS(created), parseTS(updated)
	out.Path = "src-" + shortID(out.ID)
	return out, nil
}

// ListSources returns all sources (URLs are never decrypted here).
func (s *Store) ListSources(ctx context.Context) ([]model.Source, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,name,kind,url_enc,transport,enabled,status,last_error,created_at,updated_at
		FROM sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Source, 0, 8)
	for rows.Next() {
		var (
			v                model.Source
			urlEnc           string
			enabled          int
			created, updated string
		)
		if err := rows.Scan(&v.ID, &v.Name, &v.Kind, &urlEnc, &v.Transport, &enabled,
			&v.Status, &v.LastError, &created, &updated); err != nil {
			return nil, err
		}
		v.URLEnc = urlEnc
		v.HasURL = urlEnc != ""
		if urlEnc != "" {
			v.URLMasked = "••••••"
		}
		v.Enabled = enabled == 1
		v.CreatedAt, v.UpdatedAt = parseTS(created), parseTS(updated)
		v.Path = "src-" + shortID(v.ID)
		out = append(out, v)
	}
	return out, rows.Err()
}

// UpdateSourceStatus stores the live status of a source.
func (s *Store) UpdateSourceStatus(ctx context.Context, id, status, lastErr string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sources SET status=?, last_error=?, last_seen_at=? WHERE id=?`,
		status, lastErr, ts(time.Now()), id)
	return err
}

// SourceExposesOrigin reports whether an enabled camera uses the source in
// redirect mode. Those clients are sent straight to the origin path, so the
// origin must accept reads from outside the loopback interface.
func (s *Store) SourceExposesOrigin(ctx context.Context, sourceID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM cameras WHERE source_id=? AND mode='redirect' AND enabled=1`,
		sourceID).Scan(&n)
	return n > 0, err
}

// CountCamerasBySource returns how many cameras use a source.
func (s *Store) CountCamerasBySource(ctx context.Context, sourceID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cameras WHERE source_id=?`, sourceID).Scan(&n)
	return n, err
}

// ---------------------------------------------------------------- cameras

// CreateCameras inserts cameras in one transaction.
func (s *Store) CreateCameras(ctx context.Context, cams []model.Camera) error {
	if len(cams) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO cameras (id,name,slug,source_id,group_name,enabled,on_demand,max_readers,mode,
			read_user,pass_enc,ip_allowlist,vendor,model,serial,firmware,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC()
	for i := range cams {
		c := &cams[i]
		c.ID = newID()
		c.CreatedAt, c.UpdatedAt = now, now
		if c.Mode == "" {
			c.Mode = "proxy"
		}
		if _, err := stmt.ExecContext(ctx, c.ID, c.Name, c.Slug, c.SourceID, c.Group,
			boolInt(c.Enabled), boolInt(c.OnDemand), c.MaxReaders, c.Mode,
			c.ReadUser, c.PassEnc, c.IPAllowlist, c.Vendor, c.Model, c.Serial, c.Firmware,
			ts(now), ts(now)); err != nil {
			return fmt.Errorf("insert camera %s: %w", c.Slug, err)
		}
	}
	return tx.Commit()
}

// GetCamera returns one camera by id.
func (s *Store) GetCamera(ctx context.Context, id string) (model.Camera, error) {
	row := s.db.QueryRowContext(ctx, cameraSelect+` WHERE c.id=?`, id)
	c, err := scanCamera(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Camera{}, ErrNotFound
	}
	return c, err
}

// GetCameraBySlug returns one camera by slug.
func (s *Store) GetCameraBySlug(ctx context.Context, slug string) (model.Camera, error) {
	row := s.db.QueryRowContext(ctx, cameraSelect+` WHERE c.slug=?`, slug)
	c, err := scanCamera(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Camera{}, ErrNotFound
	}
	return c, err
}

const cameraSelect = `
	SELECT c.id,c.name,c.slug,c.source_id,COALESCE(s.name,''),c.group_name,c.enabled,c.on_demand,
	       c.max_readers,c.mode,c.read_user,c.pass_enc,c.ip_allowlist,c.vendor,c.model,c.serial,c.firmware,
	       c.created_at,c.updated_at,
	       COALESCE(r.state,'unknown'),COALESCE(r.readers,0),COALESCE(r.inbound_bytes,0),
	       COALESCE(r.outbound_bytes,0),COALESCE(r.in_kbps,0),COALESCE(r.out_kbps,0),
	       COALESCE(r.last_error,''),COALESCE(r.updated_at,'')
	FROM cameras c
	LEFT JOIN sources s ON s.id=c.source_id
	LEFT JOIN runtime r ON r.camera_id=c.id`

type scanner interface {
	Scan(dest ...any) error
}

func scanCamera(sc scanner) (model.Camera, error) {
	var (
		c                         model.Camera
		enabled, onDemand         int
		created, updated          string
		rtState, rtErr, rtUpdated string
	)
	err := sc.Scan(&c.ID, &c.Name, &c.Slug, &c.SourceID, &c.SourceName, &c.Group, &enabled, &onDemand,
		&c.MaxReaders, &c.Mode, &c.ReadUser, &c.PassEnc, &c.IPAllowlist, &c.Vendor, &c.Model,
		&c.Serial, &c.Firmware, &created, &updated,
		&rtState, &c.Runtime.Readers, &c.Runtime.InboundBytes, &c.Runtime.OutboundBytes,
		&c.Runtime.InKbps, &c.Runtime.OutKbps, &rtErr, &rtUpdated)
	if err != nil {
		return model.Camera{}, err
	}
	c.Enabled = enabled == 1
	c.OnDemand = onDemand == 1
	c.CreatedAt, c.UpdatedAt = parseTS(created), parseTS(updated)
	c.Runtime.State = rtState
	c.Runtime.LastError = rtErr
	c.Runtime.UpdatedAt = parseTS(rtUpdated)
	c.HasPass = c.PassEnc != ""
	if c.HasPass {
		c.PassMasked = "••••••••"
	}
	return c, nil
}

// ListCameras returns a filtered page of cameras and the total count.
func (s *Store) ListCameras(ctx context.Context, q model.CameraQuery) ([]model.Camera, int, error) {
	where := []string{"1=1"}
	args := []any{}
	if q.Q != "" {
		where = append(where, "(c.name LIKE ? OR c.slug LIKE ? OR c.group_name LIKE ?)")
		like := "%" + q.Q + "%"
		args = append(args, like, like, like)
	}
	if q.Group != "" {
		where = append(where, "c.group_name=?")
		args = append(args, q.Group)
	}
	if q.SourceID != "" {
		where = append(where, "c.source_id=?")
		args = append(args, q.SourceID)
	}
	if q.Enabled != nil {
		where = append(where, "c.enabled=?")
		args = append(args, boolInt(*q.Enabled))
	}
	if q.State != "" {
		switch q.State {
		case "ready":
			where = append(where, "COALESCE(r.state,'unknown')='ready'")
		case "offline", "unknown", "disabled", "error":
			where = append(where, "COALESCE(r.state,'unknown')=?")
			args = append(args, q.State)
		}
	}
	cond := strings.Join(where, " AND ")

	var total int
	countQ := `SELECT COUNT(*) FROM cameras c LEFT JOIN runtime r ON r.camera_id=c.id WHERE ` + cond
	if err := s.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 50
	}
	args = append(args, q.Limit, q.Offset)
	rows, err := s.db.QueryContext(ctx, cameraSelect+` WHERE `+cond+` ORDER BY c.slug LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := make([]model.Camera, 0, q.Limit)
	for rows.Next() {
		c, err := scanCamera(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// AllCamerasForReconcile returns enabled cameras plus source fields needed to build paths.
func (s *Store) AllCamerasForReconcile(ctx context.Context) ([]model.Camera, error) {
	rows, err := s.db.QueryContext(ctx, cameraSelect+` ORDER BY c.slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Camera, 0, 256)
	for rows.Next() {
		c, err := scanCamera(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCamera persists mutable fields.
func (s *Store) UpdateCamera(ctx context.Context, c model.Camera) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
		UPDATE cameras SET name=?, group_name=?, enabled=?, on_demand=?, max_readers=?, mode=?,
			read_user=?, ip_allowlist=?, vendor=?, model=?, serial=?, firmware=?, updated_at=?
		WHERE id=?`,
		c.Name, c.Group, boolInt(c.Enabled), boolInt(c.OnDemand), c.MaxReaders, c.Mode,
		c.ReadUser, c.IPAllowlist, c.Vendor, c.Model, c.Serial, c.Firmware, ts(now), c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCameraPass stores an encrypted password.
func (s *Store) SetCameraPass(ctx context.Context, id, passEnc string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cameras SET pass_enc=?, updated_at=? WHERE id=?`,
		passEnc, ts(time.Now()), id)
	return err
}

// DeleteCamera removes a camera and its runtime row.
func (s *Store) DeleteCamera(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM runtime WHERE camera_id=?`, id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM cameras WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCameras removes several cameras.
func (s *Store) DeleteCameras(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	n := 0
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM runtime WHERE camera_id=?`, id); err != nil {
			return 0, err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM cameras WHERE id=?`, id)
		if err != nil {
			return 0, err
		}
		affected, _ := res.RowsAffected()
		n += int(affected)
	}
	return n, tx.Commit()
}

// SetCamerasEnabled enables/disables several cameras.
func (s *Store) SetCamerasEnabled(ctx context.Context, ids []string, enabled bool) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	n := 0
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE cameras SET enabled=?, updated_at=? WHERE id=?`,
			boolInt(enabled), ts(time.Now()), id)
		if err != nil {
			return 0, err
		}
		affected, _ := res.RowsAffected()
		n += int(affected)
	}
	return n, tx.Commit()
}

// SetCameraEnabled updates a single camera's enabled flag.
func (s *Store) SetCameraEnabled(ctx context.Context, id string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE cameras SET enabled=?, updated_at=? WHERE id=?`,
		boolInt(enabled), ts(time.Now()), id)
	return err
}

// ---------------------------------------------------------------- runtime

// SetRuntime upserts the live state of a camera.
func (s *Store) SetRuntime(ctx context.Context, cameraID string, rt model.Runtime) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runtime (camera_id,state,readers,inbound_bytes,outbound_bytes,in_kbps,out_kbps,last_error,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?)
		ON CONFLICT(camera_id) DO UPDATE SET
		  state=excluded.state, readers=excluded.readers, inbound_bytes=excluded.inbound_bytes,
		  outbound_bytes=excluded.outbound_bytes, in_kbps=excluded.in_kbps, out_kbps=excluded.out_kbps,
		  last_error=excluded.last_error, updated_at=excluded.updated_at`,
		cameraID, rt.State, rt.Readers, int64(rt.InboundBytes), int64(rt.OutboundBytes),
		rt.InKbps, rt.OutKbps, rt.LastError, ts(time.Now()))
	return err
}

// Overview aggregates dashboard counters.
func (s *Store) Overview(ctx context.Context) (model.Overview, error) {
	var (
		o        model.Overview
		inBytes  int64
		outBytes int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN c.enabled=1 THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN r.state='ready' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN r.state='idle' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(CASE WHEN r.state='offline' THEN 1 ELSE 0 END),0),
		       COALESCE(SUM(r.readers),0),
		       COALESCE(SUM(r.inbound_bytes),0),
		       COALESCE(SUM(r.outbound_bytes),0)
		FROM cameras c LEFT JOIN runtime r ON r.camera_id=c.id`).
		Scan(&o.CamerasTotal, &o.CamerasEnabled, &o.CamerasReady, &o.CamerasIdle, &o.CamerasOffline,
			&o.Readers, &inBytes, &outBytes)
	if err != nil {
		return o, err
	}
	_ = inBytes
	_ = outBytes
	// Bitrates are not derivable from cumulative counters here; the API merges
	// live rates produced by the reconciler into this struct.
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN status='online' THEN 1 ELSE 0 END),0) FROM sources`).
		Scan(&o.SourcesTotal, &o.SourcesOnline)
	if err != nil {
		return o, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE ts > ?`,
		ts(time.Now().Add(-time.Hour))).Scan(&o.EventsLastHour)
	return o, err
}

// ---------------------------------------------------------------- events

// AddEvent appends an event.
func (s *Store) AddEvent(ctx context.Context, level, category, subject, message string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO events (ts,level,category,subject,message) VALUES (?,?,?,?,?)`,
		ts(time.Now()), level, category, subject, message)
	return err
}

// ListEvents returns the most recent events.
func (s *Store) ListEvents(ctx context.Context, limit int) ([]model.Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id,ts,level,category,subject,message FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Event, 0, limit)
	for rows.Next() {
		var (
			e  model.Event
			tv string
		)
		if err := rows.Scan(&e.ID, &tv, &e.Level, &e.Category, &e.Subject, &e.Message); err != nil {
			return nil, err
		}
		e.TS = parseTS(tv)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents deletes events older than d.
func (s *Store) PruneEvents(ctx context.Context, d time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE ts < ?`, ts(time.Now().Add(-d)))
	return err
}

// ---------------------------------------------------------------- samples

// AddSample upserts one minute sample.
func (s *Store) AddSample(ctx context.Context, unix int64, readers int, in, out uint64, ready int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO samples (ts,readers,inbound_bytes,outbound_bytes,ready) VALUES (?,?,?,?,?)
		ON CONFLICT(ts) DO UPDATE SET readers=excluded.readers, inbound_bytes=excluded.inbound_bytes,
		  outbound_bytes=excluded.outbound_bytes, ready=excluded.ready`,
		unix, readers, int64(in), int64(out), ready)
	return err
}

// ListSamples returns up to limit newest samples in chronological order.
func (s *Store) ListSamples(ctx context.Context, limit int) ([]model.Sample, error) {
	if limit <= 0 || limit > 1440 {
		limit = 240
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts,readers,inbound_bytes,outbound_bytes,ready FROM (
			SELECT * FROM samples ORDER BY ts DESC LIMIT ?
		) ORDER BY ts`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.Sample, 0, limit)
	for rows.Next() {
		var (
			v    model.Sample
			in   int64
			outB int64
		)
		if err := rows.Scan(&v.TS, &v.Readers, &in, &outB, &v.Ready); err != nil {
			return nil, err
		}
		v.InBytes, v.OutBytes = uint64(in), uint64(outB)
		out = append(out, v)
	}
	return out, rows.Err()
}

// PruneSamples deletes samples older than cutoff (unix seconds).
func (s *Store) PruneSamples(ctx context.Context, cutoff int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM samples WHERE ts < ?`, cutoff)
	return err
}

// ---------------------------------------------------------------- helpers

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// maskURL hides credentials when echoing a configured URL.
func maskURL(raw string) string {
	if raw == "" {
		return ""
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.Index(rest, "@"); at >= 0 {
			return raw[:i+3] + "•••@" + rest[at+1:]
		}
	}
	return raw
}
