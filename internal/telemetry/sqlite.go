package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Rake-Pro/go-speedtest/internal/measure"

	// Blank import registers the pure-Go "sqlite" driver with database/sql.
	_ "modernc.org/sqlite"
)

// schema is the single results table, one column per measure.Result field.
const schema = `
CREATE TABLE IF NOT EXISTS results (
	id                   TEXT PRIMARY KEY,
	timestamp            TEXT NOT NULL,
	client_ip            TEXT NOT NULL DEFAULT '',
	user_agent           TEXT NOT NULL DEFAULT '',
	download_mbps        REAL NOT NULL DEFAULT 0,
	upload_mbps          REAL NOT NULL DEFAULT 0,
	ping_ms              REAL NOT NULL DEFAULT 0,
	jitter_ms            REAL NOT NULL DEFAULT 0,
	download_bytes       INTEGER NOT NULL DEFAULT 0,
	upload_bytes         INTEGER NOT NULL DEFAULT 0,
	download_duration_ms INTEGER NOT NULL DEFAULT 0,
	upload_duration_ms   INTEGER NOT NULL DEFAULT 0,
	streams_download     INTEGER NOT NULL DEFAULT 0,
	streams_upload       INTEGER NOT NULL DEFAULT 0,
	overhead_factor      REAL NOT NULL DEFAULT 0,
	source               TEXT NOT NULL DEFAULT '',
	server_name          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_results_timestamp ON results (timestamp DESC);
`

const columns = `id, timestamp, client_ip, user_agent, download_mbps, upload_mbps,
	ping_ms, jitter_ms, download_bytes, upload_bytes, download_duration_ms,
	upload_duration_ms, streams_download, streams_upload, overhead_factor,
	source, server_name`

// NewSQLite opens (creating if needed) the sqlite database at path, ensures
// the schema exists, and returns a Store backed by it. Uses the pure-Go
// modernc.org/sqlite driver (no cgo). WAL journaling and a busy timeout are
// applied on every connection via DSN pragmas.
func NewSQLite(path string) (Store, error) {
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("telemetry: open sqlite: %w", err)
	}
	// Single-writer homelab reality: one connection sidesteps SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("telemetry: init schema: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

type sqliteStore struct {
	db *sql.DB
}

func (s *sqliteStore) Save(ctx context.Context, r measure.Result) (string, error) {
	id, err := newID()
	if err != nil {
		return "", fmt.Errorf("telemetry: generate id: %w", err)
	}
	r.ID = id
	if r.Timestamp == "" {
		r.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO results (`+columns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Timestamp, r.ClientIP, r.UserAgent,
		r.DownloadMbps, r.UploadMbps, r.PingMs, r.JitterMs,
		r.DownloadBytes, r.UploadBytes, r.DownloadDurationMs, r.UploadDurationMs,
		r.StreamsDownload, r.StreamsUpload, r.OverheadFactor,
		r.Source, r.ServerName)
	if err != nil {
		return "", fmt.Errorf("telemetry: save: %w", err)
	}
	return id, nil
}

func (s *sqliteStore) Get(ctx context.Context, id string) (measure.Result, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+columns+` FROM results WHERE id = ?`, id)
	r, err := scanResult(row)
	if errors.Is(err, sql.ErrNoRows) {
		return measure.Result{}, ErrNotFound
	}
	if err != nil {
		return measure.Result{}, fmt.Errorf("telemetry: get: %w", err)
	}
	return r, nil
}

func (s *sqliteStore) List(ctx context.Context, limit int) ([]measure.Result, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+columns+` FROM results ORDER BY timestamp DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("telemetry: list: %w", err)
	}
	defer rows.Close()
	var out []measure.Result
	for rows.Next() {
		r, err := scanResult(rows)
		if err != nil {
			return nil, fmt.Errorf("telemetry: list scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("telemetry: list rows: %w", err)
	}
	return out, nil
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanResult(sc scanner) (measure.Result, error) {
	var r measure.Result
	err := sc.Scan(
		&r.ID, &r.Timestamp, &r.ClientIP, &r.UserAgent,
		&r.DownloadMbps, &r.UploadMbps, &r.PingMs, &r.JitterMs,
		&r.DownloadBytes, &r.UploadBytes, &r.DownloadDurationMs, &r.UploadDurationMs,
		&r.StreamsDownload, &r.StreamsUpload, &r.OverheadFactor,
		&r.Source, &r.ServerName)
	return r, err
}
