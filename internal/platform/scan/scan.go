// Package scan defines the virus-scanning contract for uploaded files and its
// two implementations: a pass-through default (so the service runs with no
// ClamAV sidecar) and a real ClamAV client speaking the clamd INSTREAM
// protocol directly over TCP -- no third-party client library, so there is
// nothing here to licence-audit.
package scan

import (
	"context"
	"errors"
	"io"
)

// Verdict is the outcome of scanning one stream.
type Verdict string

const (
	VerdictClean    Verdict = "clean"
	VerdictInfected Verdict = "infected"
	VerdictSkipped  Verdict = "skipped" // scanning was not performed
)

// ErrInfected is returned by Scan when the stream matched a signature. The
// caller (records.Service) treats this as a hard rejection: the object is
// never persisted.
var ErrInfected = errors.New("scan: file matched a virus signature")

// VirusScanner inspects a byte stream before it is trusted enough to store.
// Business code depends on this interface, never on a ClamAV client
// directly (AGENT-BRIEF §0.6): a deployment with no ClamAV sidecar still
// runs correctly with PassthroughScanner, and swapping in a cloud AV vendor
// later is a new file behind the same interface.
type VirusScanner interface {
	// Scan reads r to completion (at most sizeHint bytes) and returns a
	// verdict. It never returns VerdictInfected with a nil error -- an
	// infection is always reported as ErrInfected so callers cannot
	// accidentally ignore it by only checking the error return.
	Scan(ctx context.Context, r io.Reader, sizeHint int64) (Verdict, error)
}

// PassthroughScanner is the default VirusScanner: it consumes the stream (so
// callers can always assume Scan drains r) and reports VerdictSkipped. This
// is what "optional ClamAV scanning" means in practice -- the platform never
// silently claims a file is clean when nothing looked at it.
type PassthroughScanner struct{}

var _ VirusScanner = PassthroughScanner{}

func (PassthroughScanner) Scan(_ context.Context, r io.Reader, _ int64) (Verdict, error) {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return "", err
	}
	return VerdictSkipped, nil
}
