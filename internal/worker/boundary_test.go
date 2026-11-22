package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/harness"
)

// TestSessionCannotReachTheCodeHostToken is #48's hole from the git side: a
// session leaves hooks, config that runs commands, and a git earlier on PATH
// for the worker's next git commands. None of them may ever see the code
// host token.
func TestSessionCannotReachTheCodeHostToken(t *testing.T) {
	f := newFixture(t, "true")
	ctx := t.Context()
	ws := mustSucceed(t, f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil)))
	results := map[string]any{testRepo + "/workspace": ws.Result}

	plant := t.TempDir()
	log := filepath.Join(plant, "seen.log")
	spy := filepath.Join(plant, "spy.sh")
	// A git on PATH that records its environment and runs the real one.
	t.Setenv("PATH", plant+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.agent.RunFunc = func(c context.Context, req harness.Request) error {
		if err := writes(map[string]string{"hello.txt": "hello\n", prTextPath: "Add hello\n\nAdds hello.txt."})(c, req); err != nil {
			return err
		}
		script := "#!/bin/sh\nenv >> " + log + "\n"
		repoDir := filepath.Join(filepath.Dir(req.Dir), "repo.git")
		hooks := filepath.Join(plant, "hooks")
		for _, dir := range []string{filepath.Join(repoDir, "hooks"), hooks} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			for _, hook := range []string{"pre-push", "reference-transaction", "pre-commit", "post-commit", "post-checkout", "pre-auto-gc"} {
				if err := os.WriteFile(filepath.Join(dir, hook), []byte(script), 0o755); err != nil {
					return err
				}
			}
		}
		if err := os.WriteFile(spy, []byte(script+"exit 0\n"), 0o755); err != nil {
			return err
		}
		realGit := f.e.git
		if err := os.WriteFile(filepath.Join(plant, "git"), []byte(script+"exec "+realGit+" \"$@\"\n"), 0o755); err != nil {
			return err
		}
		cfg := "[core]\n\tbare = true\n\thooksPath = " + hooks + "\n\taskPass = " + spy +
			"\n[credential]\n\thelper = !" + spy + "\n[gc]\n\tauto = 1\n"
		return os.WriteFile(filepath.Join(repoDir, "config"), []byte(cfg), 0o600)
	}
	impl := mustSucceed(t, f.e.run(ctx, f.claim("implement", map[string]any{}, results)))
	results[testRepo+"/implement"] = impl.Result
	f.host.Pulls = nil
	mustSucceed(t, f.e.run(ctx, f.claim("open_pr", map[string]any{}, results)))

	seen, _ := os.ReadFile(log)
	if strings.Contains(string(seen), testToken) || strings.Contains(string(seen), basicAuth(testToken)) {
		t.Fatalf("code the session planted saw the code host token:\n%s", seen)
	}
}
