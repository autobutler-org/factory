package sandbox

import (
	"reflect"
	"testing"
)

func TestReconcile(t *testing.T) {
	t.Parallel()
	running := func(name, key string) Sandbox { return Sandbox{Name: name, Key: key, Phase: PhaseRunning} }
	pending := func(name, key string) Sandbox { return Sandbox{Name: name, Key: key, Phase: PhasePending} }
	ended := func(name, key string, p Phase) Sandbox { return Sandbox{Name: name, Key: key, Phase: p} }
	tests := []struct {
		name     string
		live     []Sandbox
		runnable []string
		max      int
		want     Plan
	}{
		{"nothing to do", nil, nil, 4, Plan{Free: 4}},
		{"each runnable chain gets a sandbox", nil, []string{"i/a", "i/b"}, 4, Plan{Start: []string{"i/a", "i/b"}, Free: 2}},
		{"starts are capped by max, in the order given", nil, []string{"i/a", "i/b", "i/c"}, 2, Plan{Start: []string{"i/a", "i/b"}}},
		{"a chain with a live sandbox gets no second one", []Sandbox{pending("w1", "i/a")}, []string{"i/a", "i/b"}, 4, Plan{Start: []string{"i/b"}, Free: 2}},
		{"a repeated key starts once", nil, []string{"i/a", "i/a"}, 4, Plan{Start: []string{"i/a"}, Free: 3}},
		{"ended sandboxes do not count", []Sandbox{ended("w1", "i/a", PhaseSucceeded), ended("w2", "i/b", PhaseFailed)}, []string{"i/a"}, 1, Plan{Start: []string{"i/a"}}},
		{"a live sandbox whose chain has nothing runnable may stop", []Sandbox{running("w1", "i/a"), running("w2", "i/b")}, []string{"i/b"}, 4, Plan{Stop: []Sandbox{running("w1", "i/a")}, Free: 2}},
		{"live sandboxes use up the cap", []Sandbox{running("w1", "i/a"), running("w2", "i/b")}, []string{"i/a", "i/b", "i/c"}, 2, Plan{}},
		{"over the cap starts nothing", []Sandbox{running("w1", "i/a"), running("w2", "i/b"), running("w3", "i/c")}, []string{"i/a", "i/b", "i/c", "i/d"}, 2, Plan{}},
		{"a sandbox without a key counts but never stops", []Sandbox{running("other", "")}, nil, 2, Plan{Free: 1}},
		{"zero max starts nothing", nil, []string{"i/a"}, 0, Plan{}},
		{"negative max is zero", nil, []string{"i/a"}, -1, Plan{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Reconcile(tt.live, tt.runnable, tt.max); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Reconcile = %+v, want %+v", got, tt.want)
			}
		})
	}
}
