package worker

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

const (
	// maxGateOutput caps the gate output the worker keeps and uploads. It
	// matches work.MaxStepOutputBytes, the most the server stores.
	maxGateOutput = 5 << 20
	// cutReserve is the room left under a cap for the cut marker.
	cutReserve = 128
	// gateTailBytes is how much of the end of the gate's output a failure
	// reason carries.
	gateTailBytes = 3000
	// failureBytes caps the lines a failure reason quotes as failures.
	failureBytes = 4000
	// failureLineBytes caps one quoted line.
	failureLineBytes = 300
	// failureBefore and failureAfter are the lines of context quoted
	// around a failure line.
	failureBefore, failureAfter = 3, 3
)

// capture keeps a command's output up to a cap: all of it when it fits,
// otherwise its head and its tail. It holds at most about max bytes, however
// much the command prints. It is not safe for concurrent use; exec.Cmd calls
// Write from one goroutine when Stdout and Stderr are the same capture.
type capture struct {
	headCap, tailCap int
	head, tail       []byte
	total            int64
}

func newCapture(limit int) *capture {
	keep := max(limit-cutReserve, 2)
	return &capture{headCap: keep / 2, tailCap: keep - keep/2}
}

func (c *capture) Write(p []byte) (int, error) {
	n := len(p)
	c.total += int64(n)
	if room := c.headCap - len(c.head); room > 0 {
		take := min(room, len(p))
		c.head = append(c.head, p[:take]...)
		p = p[take:]
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) > 2*c.tailCap {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-c.tailCap:]...)
	}
	return n, nil
}

// Bytes is the output kept: everything when it fit, otherwise the head and
// the tail, each cut at a line break where there is one, with a line
// between them that says how many bytes were cut.
func (c *capture) Bytes() []byte {
	tail := c.tail
	if len(tail) > c.tailCap {
		tail = tail[len(tail)-c.tailCap:]
	}
	if c.total == int64(len(c.head)+len(tail)) {
		return append(append([]byte{}, c.head...), tail...)
	}
	head := c.head
	if i := bytes.LastIndexByte(head, '\n'); i >= 0 {
		head = head[:i+1]
	}
	if i := bytes.IndexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	cut := c.total - int64(len(head)+len(tail))
	marker := fmt.Sprintf("[... %d bytes of output cut here; the first %d and the last %d bytes are kept ...]\n", cut, len(head), len(tail))
	if len(head) > 0 && head[len(head)-1] != '\n' {
		marker = "\n" + marker
	}
	out := make([]byte, 0, len(head)+len(marker)+len(tail))
	return append(append(append(out, head...), marker...), tail...)
}

// Total is how many bytes were written, kept or not.
func (c *capture) Total() int64 { return c.total }

// failureLine matches a line that commonly says what failed: a failed Go
// test or package, a panic, an error line, or a compiler diagnostic
// (file:line:column: or file(line,column): error).
var failureLine = regexp.MustCompile(`^\s*--- FAIL|^FAIL\b|panic:|\b[Ee]rror:|^\s*[\w./\\-]+\.\w+:\d+:\d+:|^\s*[\w./\\-]+\.\w+\(\d+,\d+\): error`)

// failureLines quotes the lines of out that match failureLine, each with a
// few lines of context, in order. A failure line already quoted with the
// same text is not quoted again; a gap between quoted runs is "...". At
// most limit bytes are quoted. It is a heuristic: the full output is the
// source of truth. It returns "" when no line matches.
func failureLines(out string, limit int) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var b strings.Builder
	seen := map[string]bool{}
	next := 0 // the first line not yet quoted
	for i, line := range lines {
		if !failureLine.MatchString(line) {
			continue
		}
		key := strings.TrimSpace(line)
		if seen[key] {
			continue
		}
		seen[key] = true
		from, to := max(i-failureBefore, next), min(i+failureAfter+1, len(lines))
		if to <= from {
			continue // quoted as context already
		}
		var run strings.Builder
		if b.Len() > 0 && from > next {
			run.WriteString("...\n")
		}
		for _, l := range lines[from:to] {
			if len(l) > failureLineBytes {
				l = l[:failureLineBytes] + " ..."
			}
			run.WriteString(l + "\n")
		}
		if b.Len()+run.Len() > limit {
			b.WriteString("... (more in the full output)\n")
			break
		}
		b.WriteString(run.String())
		next = to
	}
	return strings.TrimRight(b.String(), "\n")
}

// gateFailure is the reason a failed gate gives: the lines that look like
// failures, then the end of the output, then where the full output is.
// kept says where the full output is, or why it is not.
func gateFailure(command string, err error, output []byte, kept string) string {
	out := string(output)
	var b strings.Builder
	fmt.Fprintf(&b, "the gate command `%s` failed: %v.", command, err)
	if lines := failureLines(out, failureBytes); lines != "" {
		b.WriteString("\n\nLines that look like failures:\n" + lines)
	}
	b.WriteString("\n\nIts output ends:\n" + strings.TrimRight(tail(out, gateTailBytes), "\n"))
	if kept != "" {
		b.WriteString("\n\n" + kept)
	}
	return b.String()
}

// tail is the end of s, at most n bytes, from a line break where there is
// one.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i+1 < len(s) {
		s = s[i+1:]
	}
	return "...\n" + s
}
