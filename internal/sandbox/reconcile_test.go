package sandbox

import "testing"

func TestReconcile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   Counts
		want Plan
	}{
		{"nothing to do", Counts{}, Plan{}},
		{"ready work, no sandboxes", Counts{Ready: 3, Max: 5}, Plan{Start: 3}},
		{"ready work capped by max", Counts{Ready: 10, Max: 4}, Plan{Start: 4}},
		{"busy sandboxes use up the cap", Counts{Ready: 5, Busy: 3, Max: 4}, Plan{Start: 1}},
		{"idle sandboxes already cover ready work", Counts{Ready: 2, Idle: 2, Max: 5}, Plan{}},
		{"idle sandboxes cover part of ready work", Counts{Ready: 3, Idle: 1, Max: 5}, Plan{Start: 2}},
		{"surplus idle sandboxes stop", Counts{Ready: 1, Idle: 3, Max: 5}, Plan{Stop: 2}},
		{"no ready work stops every idle sandbox", Counts{Busy: 2, Idle: 2, Max: 5}, Plan{Stop: 2}},
		{"at the cap, nothing starts", Counts{Ready: 4, Busy: 2, Idle: 2, Max: 4}, Plan{}},
		{"over the cap after max was lowered stops idle first", Counts{Ready: 3, Busy: 1, Idle: 3, Max: 2}, Plan{Stop: 2}},
		{"over the cap with only busy sandboxes stops nothing", Counts{Ready: 1, Busy: 4, Max: 2}, Plan{}},
		{"zero max starts nothing and stops idle", Counts{Ready: 2, Idle: 1}, Plan{Stop: 1}},
		{"negative counts are treated as zero", Counts{Ready: -1, Busy: -2, Idle: -3, Max: -4}, Plan{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Reconcile(tt.in); got != tt.want {
				t.Errorf("Reconcile(%+v) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}
