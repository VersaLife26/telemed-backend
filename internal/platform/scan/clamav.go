package scan

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

// clamdChunkSize is the amount of file data sent per INSTREAM frame. clamd
// rejects any single frame larger than its configured StreamMaxLength, so
// this stays well under the common 25MB default regardless of our own 10MB
// upload cap.
const clamdChunkSize = 1 << 20 // 1MiB

// ClamAVScanner talks to a clamd daemon over its native INSTREAM protocol on
// a plain TCP socket. This is hand-rolled rather than pulled from a
// third-party client library: the protocol is a few dozen lines and a direct
// implementation means one fewer dependency to licence-audit and keep
// current.
//
// Protocol (see clamd(8), "INSTREAM"): send "zINSTREAM\0", then a sequence of
// chunks each prefixed by a 4-byte big-endian length, terminated by a
// zero-length chunk. clamd replies with a NUL-terminated line: "stream: OK"
// for clean, "stream: <name> FOUND" for a match.
type ClamAVScanner struct {
	addr    string
	dial    func(ctx context.Context, addr string) (net.Conn, error)
	timeout time.Duration
}

var _ VirusScanner = (*ClamAVScanner)(nil)

// NewClamAV returns a scanner that dials clamd at addr (host:port) for every
// scan. clamd's INSTREAM protocol is stateless per connection, so a fresh
// dial per file is the correct and simplest usage.
func NewClamAV(addr string, timeout time.Duration) *ClamAVScanner {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &ClamAVScanner{
		addr: addr,
		dial: func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		},
		timeout: timeout,
	}
}

func (c *ClamAVScanner) Scan(ctx context.Context, r io.Reader, _ int64) (Verdict, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	conn, err := c.dial(ctx, c.addr)
	if err != nil {
		return "", fmt.Errorf("scan: dial clamd at %s: %w", c.addr, err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if _, err := conn.Write([]byte("zINSTREAM\000")); err != nil {
		return "", fmt.Errorf("scan: send INSTREAM: %w", err)
	}

	buf := make([]byte, clamdChunkSize)
	lenPrefix := make([]byte, 4)
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			// n is always in [0, len(buf)] and len(buf) is the fixed
			// clamdChunkSize (1MiB), so this conversion never overflows
			// uint32 regardless of how large the overall upload is.
			binary.BigEndian.PutUint32(lenPrefix, uint32(n)) //nolint:gosec // bounded by clamdChunkSize
			if _, err := conn.Write(lenPrefix); err != nil {
				return "", fmt.Errorf("scan: write chunk length: %w", err)
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return "", fmt.Errorf("scan: write chunk: %w", err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", fmt.Errorf("scan: read input: %w", readErr)
		}
	}

	// Zero-length chunk signals end of stream to clamd.
	binary.BigEndian.PutUint32(lenPrefix, 0)
	if _, err := conn.Write(lenPrefix); err != nil {
		return "", fmt.Errorf("scan: write terminator: %w", err)
	}

	reply, err := bufio.NewReader(conn).ReadString('\000')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("scan: read clamd reply: %w", err)
	}
	reply = strings.TrimRight(reply, "\000\n\r ")

	switch {
	case strings.HasSuffix(reply, "OK"):
		return VerdictClean, nil
	case strings.Contains(reply, "FOUND"):
		return VerdictInfected, fmt.Errorf("%w: %s", ErrInfected, reply)
	default:
		return "", fmt.Errorf("scan: unexpected clamd reply: %q", reply)
	}
}
