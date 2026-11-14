// Package platform holds the three neighbours Shipkeeper depends on, each
// behind a small interface, with an in-memory stand-in for each. None of
// the real components exists yet; the stand-ins follow SPEC.md and are what
// the tests and the command line use.
package platform

import (
	"context"
	"sync"

	"github.com/ClearedStack/shipkeeper/internal/errs"
	"github.com/ClearedStack/shipkeeper/internal/health"
)

// Plainforge is where app.yaml is read from.
type Plainforge interface {
	// AppManifest returns the bytes of app.yaml for the named app.
	AppManifest(ctx context.Context, app string) ([]byte, error)
}

// Lazarette is the registry that holds signed image digests.
type Lazarette interface {
	// HasSignedDigest reports whether digest is one of the signed digests
	// retained for image.
	HasSignedDigest(ctx context.Context, image, digest string) (bool, error)
}

// Muster receives cleaned health reports.
type Muster interface {
	SendHealth(ctx context.Context, r health.Report) error
}

// MemoryPlainforge is the stand-in for Plainforge: app name -> app.yaml.
type MemoryPlainforge map[string][]byte

func (m MemoryPlainforge) AppManifest(_ context.Context, app string) ([]byte, error) {
	data, ok := m[app]
	if !ok {
		return nil, errs.PlainforgeNoApp
	}
	return data, nil
}

// Retained is how many versions Lazarette keeps per app (SPEC.md).
const Retained = 5

// MemoryLazarette is the stand-in for Lazarette. A digest it holds counts
// as signed; it checks no signature.
type MemoryLazarette struct {
	mu      sync.Mutex
	digests map[string][]string // image -> digests, oldest first
}

// Push records a newly signed digest for image and forgets the oldest once
// more than Retained are held.
func (l *MemoryLazarette) Push(image, digest string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.digests == nil {
		l.digests = map[string][]string{}
	}
	d := append(l.digests[image], digest)
	if len(d) > Retained {
		d = d[len(d)-Retained:]
	}
	l.digests[image] = d
}

func (l *MemoryLazarette) HasSignedDigest(_ context.Context, image, digest string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, d := range l.digests[image] {
		if d == digest {
			return true, nil
		}
	}
	return false, nil
}

// MemoryMuster is the stand-in for Muster. It keeps what it was sent, or
// fails every delivery while Down is set.
type MemoryMuster struct {
	mu      sync.Mutex
	Down    bool
	Reports []health.Report
}

func (m *MemoryMuster) SendHealth(_ context.Context, r health.Report) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Down {
		return errs.MusterTimeout
	}
	m.Reports = append(m.Reports, r)
	return nil
}
