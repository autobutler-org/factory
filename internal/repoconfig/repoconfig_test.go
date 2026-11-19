package repoconfig

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/plan"
)

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse(Path, []byte(`gate = "make check"`))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{Gate: "make check", GateTimeout: DefaultGateTimeout, SessionTimeout: DefaultSessionTimeout, ReviewRounds: 3, Merge: true}
	if cfg.Gate != want.Gate || cfg.GateTimeout != want.GateTimeout || cfg.SessionTimeout != want.SessionTimeout ||
		cfg.ReviewRounds != want.ReviewRounds || cfg.Merge != want.Merge || cfg.RequireApproval || cfg.After != nil || cfg.Image != "" {
		t.Errorf("Parse = %+v, want %+v", cfg, want)
	}
	if got, def := cfg.Plan(), plan.DefaultRepoConfig(); got.ReviewRounds != def.ReviewRounds || got.Merge != def.Merge || got.RequireApproval != def.RequireApproval {
		t.Errorf("Plan() = %+v, want the planner's default %+v", got, def)
	}
}

func TestParseEveryKey(t *testing.T) {
	cfg, err := Parse(Path, []byte(`
gate = "make check test"
gate_timeout = "5m"
session_timeout = "45m"
review_rounds = 0
require_approval = true
after = ["o/a"]
image = "ghcr.io/autobutler-org/quark-worker:1.2"

[automate]
merge = "off"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gate != "make check test" || cfg.GateTimeout != 5*time.Minute || cfg.SessionTimeout != 45*time.Minute ||
		cfg.ReviewRounds != 0 || !cfg.RequireApproval || cfg.Merge || !slices.Equal(cfg.After, []string{"o/a"}) ||
		cfg.Image != "ghcr.io/autobutler-org/quark-worker:1.2" {
		t.Errorf("Parse = %+v", cfg)
	}
	if p := cfg.Plan(); p.ReviewRounds != 0 || !p.RequireApproval || p.Merge || !slices.Equal(p.After, []string{"o/a"}) || p.Image != cfg.Image {
		t.Errorf("Plan() = %+v", p)
	}
}

func TestParseRejects(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
	}{
		{"empty file", ``, "gate is missing"},
		{"blank gate", `gate = "  "`, "gate is missing"},
		{"unknown key", "gate = \"x\"\ngaet = \"y\"", "unknown key(s) gaet"},
		{"unknown automate key", "gate = \"x\"\n[automate]\nmerj = \"on\"", "automate.merj"},
		{"bad toml", `gate = `, "fix the TOML"},
		{"gate not a string", `gate = 3`, "fix the TOML"},
		{"bad duration", "gate = \"x\"\ngate_timeout = \"soon\"", `gate_timeout = "soon" is not a positive duration`},
		{"zero duration", "gate = \"x\"\nsession_timeout = \"0s\"", "session_timeout"},
		{"negative rounds", "gate = \"x\"\nreview_rounds = -1", "must not be negative"},
		{"blank image", "gate = \"x\"\nimage = \" \"", "is empty or not one image reference"},
		{"image with spaces", "gate = \"x\"\nimage = \"ghcr.io/o/x:1 y\"", "image"},
		{"bad switch", "gate = \"x\"\n[automate]\nmerge = \"yes\"", `automate.merge = "yes" is not a legal value`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(Path, []byte(tc.data))
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), Path) {
				t.Errorf("Parse = %v, want ErrInvalid naming %s and saying %q", err, Path, tc.want)
			}
		})
	}
}
