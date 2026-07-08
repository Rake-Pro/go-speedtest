// Package payload owns the single, process-wide 1 MiB incompressible random
// buffer used to serve every download response. The buffer is generated once
// with crypto/rand at startup and thereafter treated as read-only, so it can
// be shared across all concurrent download handlers without copying.
package payload

import (
	"crypto/rand"
	"sync"
)

// ChunkSize is the size of one download chunk (1 MiB).
const ChunkSize = 1 << 20

// buf is the shared read-only random buffer, populated by Init.
var (
	buf     []byte
	initErr error
	once    sync.Once
)

// Init generates the 1 MiB random buffer with crypto/rand. It must be called
// once at startup before Buffer is used. Safe to call multiple times; only the
// first call generates.
func Init() error {
	once.Do(func() {
		b := make([]byte, ChunkSize)
		if _, err := rand.Read(b); err != nil {
			initErr = err
			return
		}
		buf = b
	})
	return initErr
}

// Buffer returns the shared read-only 1 MiB random buffer. Callers MUST NOT
// mutate it. Returns nil if Init has not been called.
func Buffer() []byte {
	return buf
}
