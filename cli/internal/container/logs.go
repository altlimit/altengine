// What a job printed.
//
// Hosted, logs come from the platform's own retention: the machine is destroyed the moment it
// exits, so its output is fetched on demand and kept for about a week. The API is best effort by
// design — a failure returns an empty page with a note rather than an error, because a job's
// RECORD (exit code, timing, cost) comes from somewhere else and must never depend on the log
// service being up.
//
// Locally the emulator is the log service. It captures the container's stdout and stderr into a
// bounded per-job buffer, which is a straight improvement on what it did before: nothing at all.
// `docker run` was started with no Stdout or Stderr set, so os/exec pointed both at /dev/null and
// a local job's output was discarded — the one difference from hosted that a developer would
// notice first and be least able to explain.
//
// BOUNDED, because a runaway job must not become a runaway process. Old lines are dropped rather
// than new ones: the tail is where the failure is.

package container

import (
	"bufio"
	"io"
	"sync"
	"time"
)

const (
	// Lines kept per job. A few thousand is enough to see how something failed and small
	// enough that a hundred finished jobs are still nothing.
	MaxLogLines = 2000
	// One pathological line can be as big as a whole log.
	MaxLogLineChars = 4000
	// Lines returned in one page, matching the hosted cap.
	LogPageSize = 500
)

// LogLine matches the hosted log shape field for field, so a caller rendering one renders the
// other.
type LogLine struct {
	Timestamp string `json:"timestamp"`
	Stream    string `json:"level,omitempty"` // "stdout" | "stderr", in hosted's `level` slot
	Message   string `json:"message"`
}

// LogPage is one page of output, oldest first.
type LogPage struct {
	Lines  []LogLine `json:"lines"`
	Cursor *int      `json:"cursor"`
	Note   string    `json:"note,omitempty"`
}

// logBuffer is a bounded ring of lines for one job, written from the runner's goroutines and
// read by HTTP handlers.
type logBuffer struct {
	mu    sync.Mutex
	lines []LogLine
	// dropped counts what fell off the front, so a truncated log can say so instead of
	// quietly looking complete.
	dropped int
}

func newLogBuffer() *logBuffer { return &logBuffer{} }

func (b *logBuffer) add(stream, msg string) {
	if len(msg) > MaxLogLineChars {
		msg = msg[:MaxLogLineChars] + "…[truncated]"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, LogLine{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Stream: stream, Message: msg})
	if len(b.lines) > MaxLogLines {
		over := len(b.lines) - MaxLogLines
		b.lines = append(b.lines[:0], b.lines[over:]...)
		b.dropped += over
	}
}

// page returns up to LogPageSize lines starting at `from`, and the next offset when there are
// more. A cursor is only offered when the page was actually full — otherwise a client would
// poll a finished job forever, one empty request at a time.
func (b *logBuffer) page(from int) LogPage {
	b.mu.Lock()
	defer b.mu.Unlock()
	if from < 0 || from > len(b.lines) {
		from = 0
	}
	end := from + LogPageSize
	if end > len(b.lines) {
		end = len(b.lines)
	}
	out := make([]LogLine, end-from)
	copy(out, b.lines[from:end])

	page := LogPage{Lines: out}
	if end < len(b.lines) {
		next := end
		page.Cursor = &next
	}
	if from == 0 && b.dropped > 0 {
		page.Note = "this job printed more than the local buffer keeps; the oldest lines were dropped"
	}
	return page
}

// writer adapts the buffer to io.Writer for one stream, splitting on newlines.
//
// bufio.Scanner would be simpler but caps a line at 64 KiB and then STOPS, silently ending the
// capture for the rest of the job. A reader loop that truncates instead keeps going.
func (b *logBuffer) writer(stream string) io.Writer {
	pr, pw := io.Pipe()
	go func() {
		r := bufio.NewReaderSize(pr, 64*1024)
		for {
			line, err := r.ReadString('\n')
			if len(line) > 0 {
				b.add(stream, trimNewline(line))
			}
			if err != nil {
				_ = pr.CloseWithError(err)
				return
			}
		}
	}()
	return pw
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
