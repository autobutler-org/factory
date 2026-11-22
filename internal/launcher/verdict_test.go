package launcher

import (
	"slices"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
)

func TestChecksVerdict(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	pass := func(n string) codehost.Check { return codehost.Check{Name: n, State: codehost.CheckPassed} }
	fail := func(n string) codehost.Check { return codehost.Check{Name: n, State: codehost.CheckFailed} }
	wait := func(n string) codehost.Check { return codehost.Check{Name: n, State: codehost.CheckPending} }
	for _, tt := range []struct {
		name       string
		required   []string
		checks     []codehost.Check
		since      time.Time
		want       string
		wantFailed []string
	}{
		{"every check passed", nil, []codehost.Check{pass("ci"), pass("lint")}, now, "pass", nil},
		{"a failure fails while others run", nil, []codehost.Check{fail("ci"), wait("lint"), fail("race")}, now, "fail", []string{"ci", "race"}},
		{"one still running", nil, []codehost.Check{pass("ci"), wait("lint")}, now, "", nil},
		{"no checks inside the grace period", nil, nil, now.Add(-time.Minute), "", nil},
		{"no checks after the grace period", nil, nil, now.Add(-3 * time.Minute), "pass", nil},
		{"only required checks count", []string{"ci"}, []codehost.Check{pass("ci"), fail("flaky")}, now, "pass", nil},
		{"a required check not reported waits", []string{"ci", "deploy"}, []codehost.Check{pass("ci")}, now.Add(-time.Hour), "", nil},
		{"a required check reported twice, once failed", []string{"ci"}, []codehost.Check{pass("ci"), fail("ci")}, now, "fail", []string{"ci"}},
		{"a required check reported twice, once running", []string{"ci"}, []codehost.Check{wait("ci"), pass("ci")}, now, "", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := checksVerdict(codehost.PullStatus{Head: "h", Required: tt.required, Checks: tt.checks}, tt.since, 2*time.Minute, now)
			if got.Verdict != tt.want || !slices.Equal(got.FailedChecks, tt.wantFailed) || got.Head != "h" {
				t.Errorf("checksVerdict = %+v, want %q failing %v", got, tt.want, tt.wantFailed)
			}
		})
	}
}
