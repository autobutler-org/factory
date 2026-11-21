package sandbox

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Caps on one read of a sandbox's log, so one request cannot pull an
// unbounded log. A read that stops at a cap says so with Logs.More.
const (
	// MaxLogLines is the most lines one read returns, and the largest tail.
	MaxLogLines = 5000
	// MaxLogBytes is the most line text one read returns.
	MaxLogBytes = 1 << 20
	// MaxLogLineBytes is the longest line returned; longer ones are cut and
	// marked Truncated.
	MaxLogLineBytes = 16 << 10
)

// NotStartedError means the sandbox exists but its worker container has not
// started, so it has no log yet. Reason and Message are the container's
// waiting state, such as ContainerCreating or ImagePullBackOff.
type NotStartedError struct {
	Reason  string
	Message string
}

func (e *NotStartedError) Error() string {
	return fmt.Sprintf("worker container has not started: %s %s", e.Reason, e.Message)
}

// LogLine is one line of a sandbox's log.
type LogLine struct {
	// Time is when the container runtime received the line.
	Time time.Time `json:"time"`
	// Text is the line, without its newline.
	Text string `json:"text"`
	// Truncated is true when the line was longer than MaxLogLineBytes and
	// Text holds only its start.
	Truncated bool `json:"truncated,omitempty"`
}

// Cursor is a position in a sandbox's log: after the first Skip lines
// stamped Time. The zero Cursor is the start of the log. Lines are counted
// within a timestamp because several can share one, and the Kubernetes API
// can only start a read at a whole second.
type Cursor struct {
	Time time.Time
	Skip int
}

// String encodes the cursor as "<unix nanoseconds>-<skip>", "0-<skip>" for
// the zero time. ParseCursor reads it back.
func (c Cursor) String() string {
	var ns int64
	if !c.Time.IsZero() {
		ns = c.Time.UnixNano()
	}
	return fmt.Sprintf("%d-%d", ns, c.Skip)
}

// ParseCursor reads a cursor written by Cursor.String.
func ParseCursor(s string) (Cursor, error) {
	rawNS, rawSkip, ok := strings.Cut(s, "-")
	ns, errNS := strconv.ParseInt(rawNS, 10, 64)
	skip, errSkip := strconv.Atoi(rawSkip)
	if !ok || errNS != nil || errSkip != nil || ns < 0 || skip < 0 {
		return Cursor{}, fmt.Errorf("cursor %q is not one this server returned", s)
	}
	if ns == 0 {
		return Cursor{Skip: skip}, nil
	}
	return Cursor{Time: time.Unix(0, ns).UTC(), Skip: skip}, nil
}

// LogQuery says which part of a log to read: the last Tail lines when
// After is nil, otherwise the lines after the cursor.
type LogQuery struct {
	Tail  int
	After *Cursor
}

// Logs is one read of a sandbox's log.
type Logs struct {
	Lines []LogLine
	// Next is the cursor after the last line returned; read with it to get
	// only newer lines.
	Next Cursor
	// More is true when a cap stopped the read before the end of the log.
	More bool
}

// readLog reads a log stream with each line stamped "<RFC3339Nano> " (what
// the Kubernetes API returns with timestamps=true), drops the lines at or
// before after, and returns the rest up to the caps. A line without a
// stamp takes the time of the line before it.
//
// ponytail: assumes the stamps never go backwards. The container runtime
// stamps stdout and stderr separately, so a line can in rare cases carry an
// earlier time than the one before it and be skipped on the next read; a
// byte offset would need a log API Kubernetes does not have.
func readLog(r io.Reader, after Cursor) (Logs, error) {
	out := Logs{Lines: []LogLine{}, Next: after}
	br := bufio.NewReaderSize(r, 64<<10)
	last := after.Time
	skipped, size := 0, 0
	for {
		raw, cut, err := readLine(br, MaxLogLineBytes+len(time.RFC3339Nano)+1)
		if errors.Is(err, io.EOF) && len(raw) == 0 && !cut {
			return out, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return out, fmt.Errorf("read log: %w", err)
		}
		line := LogLine{Time: last, Text: string(raw), Truncated: cut}
		if stamp, text, ok := strings.Cut(line.Text, " "); ok {
			if t, perr := time.Parse(time.RFC3339Nano, stamp); perr == nil {
				line.Time, line.Text = t.UTC(), text
			}
		}
		last = line.Time
		switch {
		case line.Time.Before(after.Time):
			continue
		case line.Time.Equal(after.Time) && skipped < after.Skip:
			skipped++
			continue
		}
		if len(line.Text) > MaxLogLineBytes {
			line.Text, line.Truncated = line.Text[:MaxLogLineBytes], true
		}
		if line.Truncated {
			line.Text = strings.ToValidUTF8(line.Text, "")
		}
		if len(out.Lines) == MaxLogLines || size+len(line.Text) > MaxLogBytes {
			out.More = true
			return out, nil
		}
		out.Lines = append(out.Lines, line)
		size += len(line.Text)
		if line.Time.Equal(out.Next.Time) {
			out.Next.Skip++
		} else {
			out.Next = Cursor{Time: line.Time, Skip: 1}
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
	}
}

// readLine reads one line without its newline, keeping at most limit bytes
// and discarding the rest. cut says whether bytes were discarded.
func readLine(br *bufio.Reader, limit int) (line []byte, cut bool, err error) {
	var buf bytes.Buffer
	for {
		chunk, err := br.ReadSlice('\n')
		chunk = bytes.TrimSuffix(chunk, []byte("\n"))
		if room := limit - buf.Len(); len(chunk) > room {
			chunk, cut = chunk[:room], true
		}
		buf.Write(chunk)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return buf.Bytes(), cut, err
		}
	}
}
