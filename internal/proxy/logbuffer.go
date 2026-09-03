package proxy

import (
	"bytes"
	"sync"
)

// logbuffer.go is the ring buffer every log line lands in, so the TUI pane and
// /v1/status can both show recent history without either owning it.

// logBuffer is a thread-safe ring of the most recent log lines. It implements
// io.Writer so the standard logger can target it (log.SetOutput), splitting the
// byte stream into lines and keeping at most max of them.
type logBuffer struct {
	mu      sync.Mutex
	lines   []string
	partial []byte
	max     int
}

// newLogBuffer - Builds a log buffer holding at most max lines.
func newLogBuffer(max int) *logBuffer { return &logBuffer{max: max} }

// Write - Implements io.Writer so the standard logger can be pointed straight
// at the dashboard. A write is not a line: partial input is held until its
// newline arrives, or a log line split across two writes would show up as two.
func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.partial = append(b.partial, p...)
	for {
		i := bytes.IndexByte(b.partial, '\n')
		if i < 0 {
			break
		}
		b.lines = append(b.lines, string(b.partial[:i]))
		b.partial = b.partial[i+1:]
		if len(b.lines) > b.max {
			b.lines = b.lines[len(b.lines)-b.max:]
		}
	}
	return len(p), nil
}

// tail - Returns the last n stored lines (fewer if not yet that many).
func (b *logBuffer) tail(n int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || len(b.lines) == 0 {
		return nil
	}
	if n > len(b.lines) {
		n = len(b.lines)
	}
	out := make([]string, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

// allLines - Returns a copy of all stored lines, oldest first.
func (b *logBuffer) allLines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out
}

// ── styles ──────────────────────────────────────────────────────────────────
