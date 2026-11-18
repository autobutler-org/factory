package worker

import (
	"slices"
	"strings"
)

// Ported from schmeide's internal/runner/env.go. Sessions and the gate never
// inherit the worker's environment: FACTORY_API_TOKEN, the token file path
// and anything else the worker holds stay out.

// baseEnv are the variables every session and gate sees, when the worker
// has them: what a shell, a toolchain, and an HTTPS client need to work, and
// nothing that grants access to anything.
func baseEnv() []string {
	return []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TERM", "LANG", "TZ", "TMPDIR",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
		"SSL_CERT_FILE", "SSL_CERT_DIR",
	}
}

// basePrefixes are the variable name prefixes every session and gate sees.
func basePrefixes() []string { return []string{"LC_", "XDG_"} }

// harnessPrefixes are added for agent sessions only: the harness's own login
// and settings, such as the model API key. XAI_ is the Grok CLI's login.
func harnessPrefixes() []string { return []string{"ANTHROPIC_", "CLAUDE_", "XAI_"} }

// filterEnv keeps the entries of environ whose names are in names or start
// with one of prefixes.
func filterEnv(environ, names, prefixes []string) []string {
	return slices.DeleteFunc(slices.Clone(environ), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return !slices.Contains(names, name) &&
			!slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(name, p) })
	})
}

// harnessEnv is the environment of an agent session: the base plus the
// harness's own settings.
func harnessEnv(environ []string) []string {
	return filterEnv(environ, baseEnv(), slices.Concat(basePrefixes(), harnessPrefixes()))
}

// gateEnv is the environment of the gate and of git: the base only, with
// HOME and TMPDIR pointed at scratch so nothing is read from, or left in,
// the worker's home. The model API key never reaches the gate.
func gateEnv(environ []string, scratch string) []string {
	env := slices.DeleteFunc(filterEnv(environ, baseEnv(), basePrefixes()), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return slices.Contains([]string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"}, name)
	})
	return append(env, "HOME="+scratch, "TMPDIR="+scratch)
}
