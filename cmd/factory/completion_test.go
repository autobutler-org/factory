package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestCompletionScripts(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish"} {
		t.Run(sh, func(t *testing.T) {
			golden(t, "completion/"+sh, transcript(t.Context(), "completion", sh))
		})
	}
	t.Run("unknown_shell", func(t *testing.T) {
		golden(t, "completion/unknown_shell", transcript(t.Context(), "completion", "tcsh"))
	})
	t.Run("no_shell", func(t *testing.T) {
		golden(t, "completion/no_shell", transcript(t.Context(), "completion"))
	})
}

// TestBashCompletionCompletes sources the bash script and asks it what comes
// next after each line, as bash would on tab.
func TestBashCompletionCompletes(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	script := filepath.Join(t.TempDir(), "factory.bash")
	if err := os.WriteFile(script, []byte(bashCompletion()), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		line string // the words typed; the last one is being completed
		want []string
		not  []string
	}{
		{"factory ", []string{"work", "workers", "doctor", "completion"}, []string{"list"}},
		{"factory wo", []string{"work", "worker", "workers"}, []string{"doctor"}},
		{"factory work ", []string{"list", "show", "move", "enqueue"}, []string{"work"}},
		{"factory work list --", []string{"--state", "--output", "--server"}, []string{"--reason"}},
		{"factory work list --state ready --", []string{"--source"}, nil},
		{"factory work move abc ready --", []string{"--reason"}, []string{"--state"}},
		{"factory migrate ", []string{"up", "down", "status"}, nil},
		{"factory migrate status --", []string{"--database-url"}, []string{"--output"}},
		{"factory status --", []string{"--output"}, []string{"--database-url"}},
		{"factory completion ", []string{"bash", "zsh", "fish"}, nil},
	}
	for _, tt := range tests {
		words := strings.Split(tt.line, " ")
		quoted := make([]string, len(words))
		for i, w := range words {
			quoted[i] = "'" + w + "'"
		}
		probe := "source " + script + "; COMP_WORDS=(" + strings.Join(quoted, " ") + "); COMP_CWORD=" +
			strconv.Itoa(len(words)-1) + `; _factory; printf '%s\n' "${COMPREPLY[@]}"`
		out, err := exec.CommandContext(t.Context(), bash, "-c", probe).CombinedOutput()
		if err != nil {
			t.Fatalf("%q: %v\n%s", tt.line, err, out)
		}
		got := strings.Fields(string(out))
		for _, w := range tt.want {
			if !slices.Contains(got, w) {
				t.Errorf("%q completes %v, want %s among them", tt.line, got, w)
			}
		}
		for _, w := range tt.not {
			if slices.Contains(got, w) {
				t.Errorf("%q completes %v, want no %s", tt.line, got, w)
			}
		}
	}
}

func TestZshCompletionParses(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	script := filepath.Join(t.TempDir(), "factory.zsh")
	if err := os.WriteFile(script, []byte("autoload -U +X bashcompinit && bashcompinit\n"+bashCompletion()), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), zsh, "-f", "-c", "autoload -U compinit && compinit -u && source "+script).CombinedOutput(); err != nil {
		t.Errorf("zsh cannot load the script: %v\n%s", err, out)
	}
}
