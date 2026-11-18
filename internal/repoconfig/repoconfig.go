// Package repoconfig parses and validates the factory config a repository
// tracks at [Path]. It was ported from schmeide's internal/repoconfig.
//
// The worker reads the file from the base branch, before any agent session
// runs, so a session cannot weaken its own gate. The package only parses:
// it does not read git or the file system, and it does not plan. The keys
// that shape the step graph (review_rounds, require_approval, after,
// [automate] merge) are parsed and validated here so a typo fails, and
// [Config.Plan] turns them into a plan.RepoConfig; feeding that to the
// planner is the server's job.
//
// # Schema
//
// The file is TOML. Unknown keys are an error so typos do not pass silently.
//
//	# .factory/config.toml
//
//	# Required. Shell command a change must pass before its pull request opens.
//	gate = "make check"
//
//	# Optional. How long the gate may run. A Go duration. Absent means 30m.
//	gate_timeout = "30m"
//
//	# Optional. How long one agent session may run. A Go duration. Absent
//	# means 2h.
//	session_timeout = "2h"
//
//	# Optional. Review rounds to plan; 0 skips review. Absent means 3.
//	review_rounds = 3
//
//	# Optional. Wait for a human approval before merging. Absent means false.
//	require_approval = false
//
//	# Optional. Repositories of the same work item whose chains finish first.
//	after = ["autobutler-org/quark"]
//
//	[automate]
//	# "on" merges (or enqueues) the pull request once it is ready; "off"
//	# leaves it for a person. Absent or empty means "on".
//	merge = "on"
package repoconfig

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/plan"
	"github.com/pelletier/go-toml/v2"
)

// Path is the config file location relative to the repository root.
const Path = ".factory/config.toml"

const (
	// DefaultGateTimeout is the gate deadline when gate_timeout is absent.
	DefaultGateTimeout = 30 * time.Minute
	// DefaultSessionTimeout is the session deadline when session_timeout is
	// absent.
	DefaultSessionTimeout = 2 * time.Hour
)

// Switch values for the [automate] table.
const (
	On  = "on"
	Off = "off"
)

// ErrInvalid is returned for a config file that cannot be used: malformed
// TOML, an unknown key, a missing gate, or a bad value. Retrying does not
// change it; the repository's config has to be fixed.
var ErrInvalid = errors.New("invalid repository config")

// Config is one repository's factory configuration.
type Config struct {
	// Gate is the command a change must pass.
	Gate string
	// GateTimeout is how long one gate run may take.
	GateTimeout time.Duration
	// SessionTimeout is how long one agent session may take.
	SessionTimeout time.Duration
	// ReviewRounds is how many review rounds the planner unrolls.
	ReviewRounds int
	// RequireApproval adds a wait for a human approval before merge.
	RequireApproval bool
	// Merge has the factory merge the pull request; off leaves it for a person.
	Merge bool
	// After names repositories of the same item whose chains finish first.
	After []string
}

// Plan is the part of c the planner reads.
func (c Config) Plan() plan.RepoConfig {
	return plan.RepoConfig{ReviewRounds: c.ReviewRounds, RequireApproval: c.RequireApproval, Merge: c.Merge, After: c.After}
}

// file is the TOML shape of [Config].
type file struct {
	Gate            string   `toml:"gate"`
	GateTimeout     string   `toml:"gate_timeout"`
	SessionTimeout  string   `toml:"session_timeout"`
	ReviewRounds    *int     `toml:"review_rounds"`
	RequireApproval bool     `toml:"require_approval"`
	After           []string `toml:"after"`
	Automate        struct {
		Merge string `toml:"merge"`
	} `toml:"automate"`
}

// Parse validates config bytes. name, usually the path, is used in error
// messages, which say how to fix the file.
func Parse(name string, data []byte) (Config, error) {
	var raw file
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			keys := make([]string, 0, len(strict.Errors))
			for _, e := range strict.Errors {
				keys = append(keys, strings.Join(e.Key(), "."))
			}
			return Config{}, fmt.Errorf("%w: %s: unknown key(s) %s; remove them or fix the spelling", ErrInvalid, name, strings.Join(keys, ", "))
		}
		return Config{}, fmt.Errorf("%w: %s: fix the TOML: %w", ErrInvalid, name, err)
	}
	if strings.TrimSpace(raw.Gate) == "" {
		return Config{}, fmt.Errorf("%w: %s: gate is missing or empty; set gate = \"<command>\", e.g. gate = \"make check\"", ErrInvalid, name)
	}
	gateTimeout, err := duration(name, "gate_timeout", raw.GateTimeout, DefaultGateTimeout)
	if err != nil {
		return Config{}, err
	}
	sessionTimeout, err := duration(name, "session_timeout", raw.SessionTimeout, DefaultSessionTimeout)
	if err != nil {
		return Config{}, err
	}
	rounds := plan.DefaultRepoConfig().ReviewRounds
	if raw.ReviewRounds != nil {
		rounds = *raw.ReviewRounds
	}
	if rounds < 0 {
		return Config{}, fmt.Errorf("%w: %s: review_rounds = %d must not be negative; set 0 to skip review", ErrInvalid, name, rounds)
	}
	var merge bool
	switch strings.TrimSpace(raw.Automate.Merge) {
	case On, "":
		merge = true
	case Off:
	default:
		return Config{}, fmt.Errorf("%w: %s: automate.merge = %q is not a legal value; set it to %q or %q", ErrInvalid, name, raw.Automate.Merge, On, Off)
	}
	return Config{
		Gate:            raw.Gate,
		GateTimeout:     gateTimeout,
		SessionTimeout:  sessionTimeout,
		ReviewRounds:    rounds,
		RequireApproval: raw.RequireApproval,
		Merge:           merge,
		After:           raw.After,
	}, nil
}

// duration parses a positive Go duration; empty is fallback.
func duration(name, key, raw string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%w: %s: %s = %q is not a positive duration; set a duration such as %s = \"30m\"", ErrInvalid, name, key, raw, key)
	}
	return d, nil
}
