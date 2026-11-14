// Package platform holds the three neighbours Shipkeeper depends on, each
// behind a small interface, with an in-memory stand-in for each.
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

// PlainforgeWriter is optionally implemented by Plainforge clients that can
// commit an updated app.yaml with [skip-ci] during lifecycle actions.
type PlainforgeWriter interface {
	UpdateAppManifest(ctx context.Context, app string, data []byte, summary string) error
}

// Lazarette is the registry that holds signed image digests.
type Lazarette interface {
	// HasSignedDigest reports whether digest is one of the signed digests
	// retained for image.
	HasSignedDigest(ctx context.Context, image, digest string) (bool, error)
}

// RetainedLazarette is optionally implemented by Lazarette clients that can
// list the retained signed digests for an image in oldest-to-newest order.
type RetainedLazarette interface {
	RetainedDigests(ctx context.Context, image string) ([]string, error)
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
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

func (m MemoryPlainforge) UpdateAppManifest(_ context.Context, app string, data []byte, _ string) error {
	if _, ok := m[app]; !ok {
		return errs.PlainforgeNoApp
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m[app] = cp
	return nil
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

// RetainedDigests returns a copy of the retained digests for image in
// oldest-to-newest order (at most Retained entries).
func (l *MemoryLazarette) RetainedDigests(_ context.Context, image string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	src := l.digests[image]
	if len(src) == 0 {
		return nil, nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out, nil
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
