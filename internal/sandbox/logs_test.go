package sandbox

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

// stamped is a log stream in the form the Kubernetes API returns with
// timestamps=true: one "<RFC3339Nano> <text>" per line.
func stamped(lines ...LogLine) string {
	var b strings.Builder
	for _, l := range lines {
		fmt.Fprintf(&b, "%s %s\n", l.Time.Format(time.RFC3339Nano), l.Text)
	}
	return b.String()
}

func texts(lines []LogLine) string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return strings.Join(out, ",")
}

func TestCursorRoundTrip(t *testing.T) {
	t.Parallel()
	for _, c := range []Cursor{{}, {Skip: 3}, {Time: t0.Add(123456789), Skip: 1}, {Time: t0, Skip: 2}} {
		got, err := ParseCursor(c.String())
		if err != nil || !got.Time.Equal(c.Time) || got.Skip != c.Skip {
			t.Errorf("ParseCursor(%q) = %+v, %v; want %+v", c.String(), got, err, c)
		}
	}
	for _, bad := range []string{"", "x", "1-", "-1", "1-x", "1-2-3", "1--1", "-5-0"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Errorf("ParseCursor(%q) succeeded, want an error", bad)
		}
	}
}

func TestReadLog(t *testing.T) {
	t.Parallel()
	same := t0.Add(500 * time.Millisecond)
	stream := stamped(
		LogLine{Time: t0, Text: "a"},
		LogLine{Time: same, Text: "b"},
		LogLine{Time: same, Text: "c"},
		LogLine{Time: same, Text: "d"},
		LogLine{Time: t0.Add(time.Second), Text: "e"},
	)
	tests := []struct {
		name  string
		after Cursor
		want  string
		next  Cursor
	}{
		{"from the start", Cursor{}, "a,b,c,d,e", Cursor{Time: t0.Add(time.Second), Skip: 1}},
		{"after the first line", Cursor{Time: t0, Skip: 1}, "b,c,d,e", Cursor{Time: t0.Add(time.Second), Skip: 1}},
		// Three lines share one timestamp: the cursor counts how many of
		// them were delivered, so none repeats and none is dropped.
		{"inside a shared timestamp", Cursor{Time: same, Skip: 2}, "d,e", Cursor{Time: t0.Add(time.Second), Skip: 1}},
		{"at the end", Cursor{Time: t0.Add(time.Second), Skip: 1}, "", Cursor{Time: t0.Add(time.Second), Skip: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := readLog(strings.NewReader(stream), tt.after)
			if err != nil {
				t.Fatal(err)
			}
			if texts(got.Lines) != tt.want || got.Next != tt.next || got.More {
				t.Errorf("readLog = %q next %+v more %v; want %q next %+v", texts(got.Lines), got.Next, got.More, tt.want, tt.next)
			}
		})
	}
}

// TestReadLogPollsWithoutGapsOrRepeats delivers a log one poll at a time
// while it grows, the way the API's follow does, and checks every line
// arrives exactly once.
func TestReadLogPollsWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()
	var all []LogLine
	for i := range 40 {
		// Bursts of lines in the same nanosecond and the same second.
		all = append(all, LogLine{Time: t0.Add(time.Duration(i/4) * 300 * time.Millisecond), Text: strconv.Itoa(i)})
	}
	var got []LogLine
	cursor := Cursor{}
	for n := 0; n <= len(all); n += 7 {
		// What the API server returns for sinceTime: whole seconds.
		var since []LogLine
		for _, l := range all[:n] {
			if !l.Time.Before(cursor.Time.Truncate(time.Second)) {
				since = append(since, l)
			}
		}
		res, err := readLog(strings.NewReader(stamped(since...)), cursor)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, res.Lines...)
		cursor = res.Next
	}
	if texts(got) != texts(all[:35]) {
		t.Errorf("polled %q, want %q", texts(got), texts(all[:35]))
	}
}

func TestReadLogCaps(t *testing.T) {
	t.Parallel()
	t.Run("long line", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("é", MaxLogLineBytes) // two bytes a rune
		got, err := readLog(strings.NewReader(stamped(LogLine{Time: t0, Text: long}, LogLine{Time: t0, Text: "next"})), Cursor{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Lines) != 2 || !got.Lines[0].Truncated || got.Lines[1].Truncated || got.Lines[1].Text != "next" {
			t.Fatalf("lines = %d, truncated %v; want the long line cut and the next one whole", len(got.Lines), got.Lines[0].Truncated)
		}
		if n := len(got.Lines[0].Text); n > MaxLogLineBytes || n < MaxLogLineBytes-1 || !strings.HasPrefix(long, got.Lines[0].Text) {
			t.Errorf("truncated line is %d bytes, want at most %d on a rune boundary", n, MaxLogLineBytes)
		}
	})
	t.Run("response bytes", func(t *testing.T) {
		t.Parallel()
		line := strings.Repeat("x", MaxLogLineBytes)
		var lines []LogLine
		for i := range MaxLogBytes/MaxLogLineBytes + 3 {
			lines = append(lines, LogLine{Time: t0.Add(time.Duration(i)), Text: line})
		}
		got, err := readLog(strings.NewReader(stamped(lines...)), Cursor{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Lines) != MaxLogBytes/MaxLogLineBytes || !got.More {
			t.Errorf("lines = %d more %v, want %d and more", len(got.Lines), got.More, MaxLogBytes/MaxLogLineBytes)
		}
		if got.Next != (Cursor{Time: lines[len(got.Lines)-1].Time, Skip: 1}) {
			t.Errorf("next = %+v, want the last delivered line", got.Next)
		}
	})
	t.Run("line count", func(t *testing.T) {
		t.Parallel()
		lines := make([]LogLine, MaxLogLines+1)
		for i := range lines {
			lines[i] = LogLine{Time: t0, Text: "x"}
		}
		got, err := readLog(strings.NewReader(stamped(lines...)), Cursor{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Lines) != MaxLogLines || !got.More || got.Next != (Cursor{Time: t0, Skip: MaxLogLines}) {
			t.Errorf("lines = %d more %v next %+v, want %d, more, and a cursor inside the timestamp", len(got.Lines), got.More, got.Next, MaxLogLines)
		}
	})
}

func TestReadLogUnstampedLine(t *testing.T) {
	t.Parallel()
	got, err := readLog(strings.NewReader(stamped(LogLine{Time: t0, Text: "a"})+"no stamp\n"), Cursor{})
	if err != nil {
		t.Fatal(err)
	}
	// A line without a timestamp takes the one before it, so the cursor
	// still counts it.
	if texts(got.Lines) != "a,no stamp" || !got.Lines[1].Time.Equal(t0) || got.Next != (Cursor{Time: t0, Skip: 2}) {
		t.Errorf("readLog = %+v next %+v", got.Lines, got.Next)
	}
}

func TestNotStartedError(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrap: %w", &NotStartedError{Reason: "ImagePullBackOff", Message: "pull failed"})
	var ns *NotStartedError
	if !errors.As(err, &ns) || ns.Reason != "ImagePullBackOff" || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Errorf("err = %v, want a NotStartedError naming its reason", err)
	}
}
