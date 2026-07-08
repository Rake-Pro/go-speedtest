// Package telemetry persists completed test results. The Store interface has
// two backends: a no-op "none" store (default) and a pure-Go sqlite store
// (see sqlite.go). Results use the canonical measure.Result schema.
package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/Rake-Pro/go-speedtest/internal/measure"
)

// ErrNotFound is returned by Get when no result exists for the given id.
var ErrNotFound = errors.New("result not found")

// ErrNotImplemented is returned by stubbed backends until wave-2 fills them in.
var ErrNotImplemented = errors.New("not implemented")

// Store persists and retrieves speed-test results.
type Store interface {
	// Save persists r (assigning/returning its id) and returns the id.
	Save(ctx context.Context, r measure.Result) (string, error)
	// Get returns the stored result for id, or ErrNotFound.
	Get(ctx context.Context, id string) (measure.Result, error)
	// List returns up to limit most-recent results (newest first).
	List(ctx context.Context, limit int) ([]measure.Result, error)
	// Close releases any backend resources.
	Close() error
}

// newID returns a random 32-hex-char identifier for a stored result.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// NewNone returns a Store that discards writes (Save still returns a
// generated id so callers behave uniformly) and returns ErrNotFound on reads.
// It is the default backend.
func NewNone() Store { return noneStore{} }

type noneStore struct{}

func (noneStore) Save(ctx context.Context, r measure.Result) (string, error) {
	_ = ctx
	_ = r
	return newID()
}

func (noneStore) Get(ctx context.Context, id string) (measure.Result, error) {
	_ = ctx
	_ = id
	return measure.Result{}, ErrNotFound
}

func (noneStore) List(ctx context.Context, limit int) ([]measure.Result, error) {
	_ = ctx
	_ = limit
	return nil, nil
}

func (noneStore) Close() error { return nil }
