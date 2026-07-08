package telemetry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rake-Pro/go-speedtest/internal/measure"
)

func TestNoneStore(t *testing.T) {
	ctx := context.Background()
	s := NewNone()

	id, err := s.Save(ctx, measure.Result{DownloadMbps: 100})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if id == "" {
		t.Fatal("Save returned empty id")
	}

	if _, err := s.Get(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}

	list, err := s.List(ctx, 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List returned %d results, want 0", len(list))
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func newTestSQLite(t *testing.T) (Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telemetry.db")
	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestSQLiteRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSQLite(t)

	tests := []struct {
		name string
		in   measure.Result
	}{
		{
			name: "full result",
			in: measure.Result{
				Timestamp:          "2026-07-08T10:00:00Z",
				ClientIP:           "192.0.2.10",
				UserAgent:          "go-speedtest-cli/1.0",
				DownloadMbps:       941.5,
				UploadMbps:         38.2,
				PingMs:             1.7,
				JitterMs:           0.4,
				DownloadBytes:      1764753408,
				UploadBytes:        71663616,
				DownloadDurationMs: 15000,
				UploadDurationMs:   15000,
				StreamsDownload:    6,
				StreamsUpload:      3,
				OverheadFactor:     1.06,
				Source:             measure.SourceCLI,
				ServerName:         "homelab",
			},
		},
		{
			name: "sparse result",
			in: measure.Result{
				Timestamp: "2026-07-08T11:00:00Z",
				Source:    measure.SourceWeb,
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id, err := s.Save(ctx, tc.in)
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
			if id == "" {
				t.Fatal("Save returned empty id")
			}
			got, err := s.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			want := tc.in
			want.ID = id
			if got != want {
				t.Fatalf("Get mismatch\n got: %+v\nwant: %+v", got, want)
			}
		})
	}
}

func TestSQLiteSaveStampsTimestamp(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSQLite(t)

	id, err := s.Save(ctx, measure.Result{Source: measure.SourceAPI})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	ts, err := time.Parse(time.RFC3339, got.Timestamp)
	if err != nil {
		t.Fatalf("stamped timestamp %q is not RFC3339: %v", got.Timestamp, err)
	}
	if d := time.Since(ts); d < 0 || d > time.Minute {
		t.Fatalf("stamped timestamp %v not near now", ts)
	}
}

func TestSQLiteGetNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSQLite(t)
	if _, err := s.Get(ctx, "does-not-exist"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestSQLiteListOrderingAndLimit(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestSQLite(t)

	stamps := []string{
		"2026-07-08T10:00:00Z",
		"2026-07-08T12:00:00Z",
		"2026-07-08T11:00:00Z",
	}
	for _, ts := range stamps {
		if _, err := s.Save(ctx, measure.Result{Timestamp: ts, Source: measure.SourceWeb}); err != nil {
			t.Fatalf("Save(%s): %v", ts, err)
		}
	}

	tests := []struct {
		name  string
		limit int
		want  []string // expected timestamps, newest first
	}{
		{"limit larger than rows", 10, []string{"2026-07-08T12:00:00Z", "2026-07-08T11:00:00Z", "2026-07-08T10:00:00Z"}},
		{"limit truncates", 2, []string{"2026-07-08T12:00:00Z", "2026-07-08T11:00:00Z"}},
		{"limit one", 1, []string{"2026-07-08T12:00:00Z"}},
		{"limit zero", 0, nil},
		{"limit negative", -3, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.List(ctx, tc.limit)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("List returned %d results, want %d", len(got), len(tc.want))
			}
			for i, want := range tc.want {
				if got[i].Timestamp != want {
					t.Fatalf("List[%d].Timestamp = %s, want %s", i, got[i].Timestamp, want)
				}
			}
		})
	}
}

func TestSQLiteReopenPersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "telemetry.db")

	s, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("NewSQLite: %v", err)
	}
	id, err := s.Save(ctx, measure.Result{Timestamp: "2026-07-08T10:00:00Z"})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := NewSQLite(path)
	if err != nil {
		t.Fatalf("reopen NewSQLite: %v", err)
	}
	defer s2.Close()
	got, err := s2.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.ID != id {
		t.Fatalf("Get after reopen ID = %s, want %s", got.ID, id)
	}
}
