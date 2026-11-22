package worker

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCapture(t *testing.T) {
	t.Run("keeps output that fits whole", func(t *testing.T) {
		for _, n := range []int{0, 1, 1000 - cutReserve} {
			c := newCapture(1000)
			in := strings.Repeat("x", n)
			_, _ = c.Write([]byte(in))
			if got := string(c.Bytes()); got != in || c.Total() != int64(n) {
				t.Errorf("%d bytes: kept %d, total %d", n, len(got), c.Total())
			}
		}
	})
	t.Run("keeps the head and the tail past the cap, with a marker", func(t *testing.T) {
		const limit = 1000
		c := newCapture(limit)
		var in strings.Builder
		for i := range 500 {
			fmt.Fprintf(&in, "line %03d\n", i)
		}
		// Many small writes, as a command prints.
		for line := range strings.SplitAfterSeq(in.String(), "\n") {
			_, _ = c.Write([]byte(line))
		}
		got := string(c.Bytes())
		if len(got) > limit {
			t.Errorf("kept %d bytes, more than the cap %d", len(got), limit)
		}
		if !strings.HasPrefix(got, "line 000\nline 001\n") || !strings.HasSuffix(got, "line 498\nline 499\n") {
			t.Errorf("the head or the tail is missing:\n%s", got)
		}
		head, rest, ok := strings.Cut(got, "[... ")
		if !ok || !strings.HasSuffix(head, "\n") {
			t.Fatalf("no marker on a line of its own:\n%s", got)
		}
		marker, tail, _ := strings.Cut(rest, "\n")
		want := fmt.Sprintf("%d bytes of output cut here; the first %d and the last %d bytes are kept ...]", in.Len()-len(head)-len(tail), len(head), len(tail))
		if marker != want {
			t.Errorf("marker = %q, want %q", marker, want)
		}
		if !strings.HasPrefix(tail, "line ") {
			t.Errorf("the tail does not start at a line: %q", tail[:20])
		}
		if c.Total() != int64(in.Len()) {
			t.Errorf("total = %d, want %d", c.Total(), in.Len())
		}
		if held := cap(c.tail) + cap(c.head); held > 4*limit {
			t.Errorf("holds %d bytes for a %d byte cap", held, limit)
		}
	})
	t.Run("one write larger than the cap", func(t *testing.T) {
		c := newCapture(300)
		_, _ = c.Write([]byte(strings.Repeat("a", 1000) + strings.Repeat("b", 1000)))
		got := string(c.Bytes())
		if len(got) > 300 || !strings.HasPrefix(got, "aaa") || !strings.HasSuffix(got, "bbb") || !strings.Contains(got, "\n[... ") {
			t.Errorf("kept %d bytes: %q", len(got), got)
		}
	})
	t.Run("the worker's cap is the server's", func(t *testing.T) {
		c := newCapture(maxGateOutput)
		chunk := []byte(strings.Repeat("=== RUN   TestSomething\n", 1000))
		for c.Total() < 3*maxGateOutput {
			_, _ = c.Write(chunk)
		}
		if got := len(c.Bytes()); got > 5<<20 {
			t.Errorf("kept %d bytes of 15 MiB, more than 5 MiB", got)
		}
	})
}

func TestFailureLines(t *testing.T) {
	var verbose strings.Builder
	verbose.WriteString("=== RUN   TestEarly\n    early_test.go:12: got 1, want 2\n--- FAIL: TestEarly (0.00s)\n")
	for i := range 20000 {
		fmt.Fprintf(&verbose, "=== RUN   TestPass%d\n--- PASS: TestPass%d (0.00s)\n", i, i)
	}
	for _, line := range []string{"FAIL", "FAIL\tgithub.com/o/r/pkg\t0.4s", "ok  \tgithub.com/o/r/vfs\t0.423s", "FAIL", "make: *** [Makefile:1475: test] Error 1"} {
		verbose.WriteString(line + "\n")
	}

	tests := []struct {
		name, out string
		want      []string
		not       []string
	}{
		{
			name: "an early Go test failure in a megabyte of passes",
			out:  verbose.String(),
			want: []string{"--- FAIL: TestEarly", "early_test.go:12: got 1, want 2", "FAIL\tgithub.com/o/r/pkg"},
			not:  []string{"TestPass100"},
		},
		{
			name: "a compiler diagnostic",
			out:  "# github.com/o/r\n./main.go:12:3: undefined: foo\n",
			want: []string{"./main.go:12:3: undefined: foo"},
		},
		{
			name: "a TypeScript diagnostic",
			out:  "src/app.ts(4,7): error TS2322: Type 'string' is not assignable\n",
			want: []string{"src/app.ts(4,7): error TS2322"},
		},
		{
			name: "a panic",
			out:  "ok\npanic: runtime error: index out of range\n\ngoroutine 1 [running]:\n",
			want: []string{"panic: runtime error", "goroutine 1 [running]:"},
		},
		{
			name: "error lines",
			out:  "step 1\nerror: linker failed\nTraceback\nValueError: bad\n",
			want: []string{"error: linker failed", "ValueError: bad"},
		},
		{name: "nothing failed", out: "ok  \tgithub.com/o/r\t0.1s\nPASS\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failureLines(tt.out, failureBytes)
			if len(tt.want) == 0 && got != "" {
				t.Errorf("failureLines = %q, want none", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("failureLines does not quote %q:\n%s", w, got)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(got, n) {
					t.Errorf("failureLines quotes %q:\n%s", n, got)
				}
			}
			if len(got) > failureBytes+100 {
				t.Errorf("quoted %d bytes, past the cap", len(got))
			}
		})
	}

	t.Run("a repeated failure line is quoted once", func(t *testing.T) {
		out := strings.Repeat("a\nb\nc\nd\nFAIL\n", 10)
		if got := failureLines(out, failureBytes); strings.Count(got, "FAIL") != 1 {
			t.Errorf("failureLines = %q, want FAIL once", got)
		}
	})
	t.Run("runs apart are separated, runs that touch are joined", func(t *testing.T) {
		lines := make([]string, 40)
		for i := range lines {
			lines[i] = fmt.Sprintf("l%d", i)
		}
		lines[5], lines[8], lines[30] = "error: one", "error: two", "error: three"
		got := failureLines(strings.Join(lines, "\n"), failureBytes)
		want := "l2\nl3\nl4\nerror: one\nl6\nl7\nerror: two\nl9\nl10\nl11\n...\nl27\nl28\nl29\nerror: three\nl31\nl32\nl33"
		if got != want {
			t.Errorf("failureLines =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("the cap stops quoting and says so", func(t *testing.T) {
		var out strings.Builder
		for i := range 1000 {
			fmt.Fprintf(&out, "a\nb\nc\nerror: number %d\n", i)
		}
		got := failureLines(out.String(), 500)
		if len(got) > 500+50 || !strings.Contains(got, "error: number 0") || !strings.HasSuffix(got, "(more in the full output)") {
			t.Errorf("failureLines with a 500 byte cap = %d bytes:\n%s", len(got), got)
		}
	})
	t.Run("a long line is shortened", func(t *testing.T) {
		got := failureLines("error: "+strings.Repeat("y", 5000), failureBytes)
		if len(got) > failureLineBytes+10 || !strings.HasSuffix(got, " ...") {
			t.Errorf("failureLines = %d bytes", len(got))
		}
	})
}

func TestGateFailure(t *testing.T) {
	out := "--- FAIL: TestX (0.00s)\n" + strings.Repeat("ok  \tpkg\t0.1s\n", 1000) + "FAIL\n"
	got := gateFailure("make check", errors.New("exit status 2"), []byte(out), "The full output is kept.")
	lead, rest, _ := strings.Cut(got, "\n\nLines that look like failures:\n")
	if lead != "the gate command `make check` failed: exit status 2." {
		t.Errorf("lead = %q", lead)
	}
	failures, rest, _ := strings.Cut(rest, "\n\nIts output ends:\n")
	if !strings.HasPrefix(failures, "--- FAIL: TestX") {
		t.Errorf("failures = %q", failures)
	}
	end, kept, _ := strings.Cut(rest, "\n\n")
	if !strings.HasPrefix(end, "...\nok  ") || !strings.HasSuffix(end, "\nFAIL") || len(end) > gateTailBytes+10 {
		t.Errorf("tail = %q", end)
	}
	if kept != "The full output is kept." {
		t.Errorf("kept = %q", kept)
	}
	if got := gateFailure("true", errors.New("exit status 1"), []byte("short\n"), ""); got != "the gate command `true` failed: exit status 1.\n\nIts output ends:\nshort" {
		t.Errorf("without failure lines = %q", got)
	}
}
