package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/harness"
)

const (
	testRepo   = "o/r"
	testBranch = "b2e9c0de_add-hello"
	testKey    = "b2e9c0de-0000-4000-8000-000000000001/o/r"
	testToken  = "ghs_steptoken000000000000000000000000000"
)

// git runs git in dir for a test.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newOrigin creates a bare repository o/r under a new directory, with main
// holding files, and returns the directory, the base of its clone URLs.
func newOrigin(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, testRepo+".git")
	if err := os.MkdirAll(bare, 0o750); err != nil {
		t.Fatal(err)
	}
	git(t, bare, "init", "-q", "--bare", "-b", "main")
	seed := t.TempDir()
	git(t, seed, "init", "-q", "-b", "main")
	for name, body := range files {
		path := filepath.Join(seed, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", bare, "main")
	return root
}

// fixture is an executor wired to a local origin, a fake code host, a fake
// agent, and a fake factory server that hands out tokens and takes stages.
type fixture struct {
	t      *testing.T
	origin string
	e      *executor
	host   *codehost.Fake
	agent  *harness.Fake
	stages []string
	// outputs are the gate outputs uploaded, by step ID.
	outputs map[string][]byte
	// refuseOutputs makes the server refuse gate output uploads.
	refuseOutputs bool
	// identity, when set, is the commit_identity JSON the token reply
	// carries.
	identity string
}

func newFixture(t *testing.T, gate string) *fixture {
	t.Helper()
	f := &fixture{t: t, host: &codehost.Fake{Repo: testRepo}, agent: &harness.Fake{}, outputs: map[string][]byte{}}
	f.origin = newOrigin(t, map[string]string{
		"README.md":            "hello\n",
		".factory/config.toml": fmt.Sprintf("gate = %q\n", gate),
	})
	srv := http.NewServeMux()
	srv.HandleFunc("POST /api/v1/work/{id}/codehost-token", func(w http.ResponseWriter, r *http.Request) {
		identity := ""
		if f.identity != "" {
			identity = `,"commit_identity":` + f.identity
		}
		_, _ = w.Write([]byte(`{"repo":"o/r","token":"` + testToken + `"` + identity + `}`))
	})
	srv.HandleFunc("POST /api/v1/steps/{id}/stage", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stage string `json:"stage"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.stages = append(f.stages, body.Stage)
		_, _ = w.Write([]byte(`{}`))
	})
	srv.HandleFunc("PUT /api/v1/steps/{id}/output", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("worker_id") != "w" {
			t.Errorf("output uploaded for worker %q", r.URL.Query().Get("worker_id"))
		}
		if f.refuseOutputs {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"internal","message":"internal error"}}`))
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.outputs[r.PathValue("id")] = body
		_, _ = w.Write([]byte(`{}`))
	})
	_, client := newRecorder(t, nil)
	client.URL = serve(t, srv)
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	f.e = &executor{workerID: "w", git: gitBin, exhausted: map[string]bool{}, cfg: Config{
		Client:    client,
		WorkDir:   t.TempDir(),
		GitURL:    "file://" + f.origin,
		Harnesses: []harness.Harness{f.agent},
		Host: func(repo string, tokens func(context.Context) (string, error)) codehost.Host {
			if token, err := tokens(t.Context()); err != nil || token != testToken {
				t.Errorf("code host token = %q, %v", token, err)
			}
			return f.host
		},
		IdleLimit: DefaultIdleLimit,
		Log:       testLogger(t),
	}}
	return f
}

// claim is a claim on a step of kind with the given results and outputs.
func (f *fixture) claim(kind string, params map[string]any, results map[string]any, outputs ...Output) Claim {
	f.t.Helper()
	params["repo"] = testRepo
	p, _ := json.Marshal(params)
	raw := map[string]json.RawMessage{}
	for k, v := range results {
		raw[k], _ = json.Marshal(v)
	}
	return Claim{
		Step:    Step{ID: "s", Key: testRepo + "/" + kind, Kind: kind, Params: p, SandboxKey: testKey},
		Item:    Item{ID: "b2e9c0de-0000-4000-8000-000000000001", Title: "add hello", Body: "Add hello.txt.", Repos: []string{testRepo}, Source: "jira", ExternalURL: "https://jira.test/browse/F-1"},
		Results: raw,
		Outputs: outputs,
	}
}

// writes makes the fake agent write files into its checkout.
func writes(files map[string]string) func(context.Context, harness.Request) error {
	return func(_ context.Context, req harness.Request) error {
		for name, body := range files {
			path := filepath.Join(req.Dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
				return err
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				return err
			}
		}
		return nil
	}
}

func mustSucceed(t *testing.T, o outcome) Result {
	t.Helper()
	r := o.report()
	if r.Outcome != "succeeded" {
		t.Fatalf("outcome = %+v, want succeeded", r)
	}
	return r
}

func asMap(t *testing.T, v any) map[string]any {
	t.Helper()
	data, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// chain runs workspace and implement and returns the results so far.
func (f *fixture) chain() map[string]any {
	t := f.t
	t.Helper()
	ctx := t.Context()
	ws := mustSucceed(t, f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil)))
	results := map[string]any{testRepo + "/workspace": ws.Result}
	f.agent.RunFunc = writes(map[string]string{"hello.txt": "hello\n", prTextPath: "Add hello\n\nAdds hello.txt."})
	impl := mustSucceed(t, f.e.run(ctx, f.claim("implement", map[string]any{}, results)))
	results[testRepo+"/implement"] = impl.Result
	return results
}

// wantSignOff is the trailer every commit the worker makes carries when the
// server sends no commit identity.
const wantSignOff = "Signed-off-by: factory <factory@users.noreply.github.com>"

// TestCommitsAsTheServersIdentity authors, commits and signs off as the
// identity the token reply carries, and still reads the pull request text
// without the sign-off.
func TestCommitsAsTheServersIdentity(t *testing.T) {
	f := newFixture(t, "true")
	f.identity = `{"name":"app[bot]","email":"7+app[bot]@users.noreply.github.com"}`
	results := f.chain()
	bare := filepath.Join(f.origin, testRepo+".git")
	const who = "app[bot] <7+app[bot]@users.noreply.github.com>"
	if got := git(t, bare, "log", "-1", "--format=%an <%ae>|%cn <%ce>", testBranch); got != who+"|"+who {
		t.Errorf("author|committer = %q, want %s for both", got, who)
	}
	if msg := git(t, bare, "log", "-1", "--format=%B", testBranch); msg != "Add hello\n\nAdds hello.txt.\n\nSigned-off-by: "+who {
		t.Errorf("commit message = %q, want it signed off by %s", msg, who)
	}
	s, err := newStepRun(f.e, f.claim("open_pr", map[string]any{}, results))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := s.tree(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, body, err := tr.firstMessage(t.Context()); err != nil || body != "Adds hello.txt." {
		t.Errorf("firstMessage body = %q, %v; want it without the sign-off", body, err)
	}
}

func TestChainFromWorkspaceToReview(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "gate-ran")
	f := newFixture(t, "test -f hello.txt && touch "+marker)
	ctx := t.Context()

	ws := asMap(t, mustSucceed(t, f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil))).Result)
	if ws["branch"] != testBranch || ws["base"] != "main" || ws["head"] == "" {
		t.Fatalf("workspace result = %v", ws)
	}
	results := f.chain()
	bare := filepath.Join(f.origin, testRepo+".git")
	if head := asMap(t, results[testRepo+"/implement"])["head"]; git(t, bare, "rev-parse", testBranch) != head {
		t.Errorf("implement pushed %s, result head %v", git(t, bare, "rev-parse", testBranch), head)
	}
	if msg := git(t, bare, "log", "-1", "--format=%B", testBranch); msg != "Add hello\n\nAdds hello.txt.\n\n"+wantSignOff {
		t.Errorf("commit message = %q, want the session's pull request text, signed off", msg)
	}
	if prompt := f.agent.Requests[0].Prompt; !strings.Contains(prompt, "https://jira.test/browse/F-1") || !strings.Contains(prompt, "add hello") || strings.Contains(strings.ToLower(prompt), "issue") {
		t.Errorf("brief = %q, want the item's source and title and no tracker assumption", prompt)
	}
	for _, e := range f.agent.Requests[0].Env {
		if strings.HasPrefix(e, "FACTORY_") {
			t.Errorf("session env has %s", e)
		}
	}

	mustSucceed(t, f.e.run(ctx, f.claim("gate", map[string]any{}, results)))
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the gate did not run: %v", err)
	}
	mustSucceed(t, f.e.run(ctx, f.claim("secret_scan", map[string]any{}, results)))

	sibling := Output{Repo: "o/other", Kind: "pull_request", URL: "https://github.test/o/other/pull/7", Status: "open"}
	pr := mustSucceed(t, f.e.run(ctx, f.claim("open_pr", map[string]any{}, results, sibling)))
	if len(f.host.Pulls) != 1 || f.host.Pulls[0].Head.Ref != testBranch || f.host.Pulls[0].Base.Ref != "main" {
		t.Fatalf("pulls = %+v", f.host.Pulls)
	}
	url := f.host.Pulls[0].HTMLURL
	if asMap(t, pr.Result)["url"] != url || len(pr.Outputs) != 1 || pr.Outputs[0] != (Output{Repo: testRepo, Kind: "pull_request", Branch: testBranch, URL: url, Status: "open"}) {
		t.Errorf("open_pr = %+v", pr)
	}

	f.agent.RunFunc = writes(map[string]string{reviewTextPath: "pass", "stray.txt": "x"})
	rev := asMap(t, mustSucceed(t, f.e.run(ctx, f.claim("review", map[string]any{"round": 1, "last": false}, results))).Result)
	if rev["verdict"] != "pass" {
		t.Errorf("review = %v", rev)
	}
	if !strings.HasPrefix(f.agent.Requests[len(f.agent.Requests)-1].Prompt, reviewLead) {
		t.Error("the review prompt does not start with the review lead")
	}
	root, _ := (&stepRun{e: f.e, claim: f.claim("review", map[string]any{}, nil)}).root()
	if _, err := os.Stat(filepath.Join(root, "tree", "stray.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the review left a file behind: %v", err)
	}
	want := []string{stageWorktreeReady, stageWorktreeReady, stageSessionRunning, stageSessionFinished, stageGatePassed, stagePROpened, stageReview}
	if strings.Join(f.stages, ",") != strings.Join(want, ",") {
		t.Errorf("stages = %v, want %v", f.stages, want)
	}
}

func TestStepsResumeInAFreshSandbox(t *testing.T) {
	f := newFixture(t, "test -f hello.txt")
	results := f.chain()
	f.e.cfg.WorkDir = t.TempDir() // a new Job: the disk is empty
	ctx := t.Context()
	mustSucceed(t, f.e.run(ctx, f.claim("gate", map[string]any{}, results)))
	mustSucceed(t, f.e.run(ctx, f.claim("open_pr", map[string]any{}, results)))
	if len(f.host.Pulls) != 1 {
		t.Errorf("pulls = %+v", f.host.Pulls)
	}
}

func TestImplementRunAgainAfterPushSkipsTheSession(t *testing.T) {
	f := newFixture(t, "true")
	results := f.chain()
	delete(results, testRepo+"/implement")
	sessions := len(f.agent.Requests)
	f.e.cfg.WorkDir = t.TempDir()
	r := mustSucceed(t, f.e.run(t.Context(), f.claim("implement", map[string]any{}, results)))
	if len(f.agent.Requests) != sessions {
		t.Error("a retried implement ran the session again over a pushed branch")
	}
	if asMap(t, r.Result)["head"] == "" {
		t.Errorf("result = %v", r.Result)
	}
}

func TestOpenPRDoesNotOpenASecond(t *testing.T) {
	f := newFixture(t, "true")
	results := f.chain()
	ctx := t.Context()

	// An attempt opened the pull request and died before recording it.
	f.host.Pulls = []codehost.Pull{{Number: 1, HTMLURL: "https://github.test/o/r/pull/1", State: "open"}}
	f.host.Pulls[0].Head.Ref = testBranch
	r := mustSucceed(t, f.e.run(ctx, f.claim("open_pr", map[string]any{}, results)))
	if len(f.host.Pulls) != 1 || r.Outputs[0].URL != "https://github.test/o/r/pull/1" {
		t.Errorf("found pull: pulls %+v, result %+v", f.host.Pulls, r)
	}

	// The output is recorded: nothing is pushed or opened.
	recorded := Output{Repo: testRepo, Kind: "pull_request", Branch: testBranch, URL: "https://github.test/o/r/pull/1", Status: "open"}
	f.e.cfg.GitURL = "file:///nonexistent"
	r = mustSucceed(t, f.e.run(ctx, f.claim("open_pr", map[string]any{}, results, recorded)))
	if asMap(t, r.Result)["url"] != recorded.URL || len(f.host.Pulls) != 1 {
		t.Errorf("recorded pull: %+v", r)
	}
}

func TestFailures(t *testing.T) {
	ctx := t.Context()
	t.Run("gate failure is final and carries the output", func(t *testing.T) {
		f := newFixture(t, "echo the gate broke; exit 3")
		r := f.e.run(ctx, f.claim("gate", map[string]any{}, f.chain())).report()
		if r.Outcome != "failed" || r.Retryable || !strings.Contains(r.Reason, "the gate broke") || !strings.Contains(r.Reason, "exit status 3") {
			t.Errorf("gate = %+v", r)
		}
		if string(f.outputs["s"]) != "the gate broke\n" {
			t.Errorf("kept output = %q", f.outputs["s"])
		}
	})
	t.Run("a gate that prints a megabyte and fails early", func(t *testing.T) {
		// A failing test first, then a megabyte of passing ones, as go test -v prints.
		gate := `printf '=== RUN   TestEarly\n    early_test.go:9: got 1, want 2\n--- FAIL: TestEarly (0.00s)\n'; ` +
			`i=0; while [ $i -lt 30000 ]; do printf '=== RUN   TestPass\n--- PASS: TestPass (0.00s)\n'; i=$((i+1)); done; ` +
			`printf 'FAIL\tgithub.com/o/r\t0.4s\n'; exit 1`
		f := newFixture(t, gate)
		r := f.e.run(ctx, f.claim("gate", map[string]any{}, f.chain())).report()
		if r.Outcome != "failed" || r.Retryable {
			t.Fatalf("gate = %+v", r)
		}
		for _, want := range []string{"--- FAIL: TestEarly", "early_test.go:9: got 1, want 2", "GET /api/v1/steps/s/output"} {
			if !strings.Contains(r.Reason, want) {
				t.Errorf("the reason does not name %q:\n%s", want, r.Reason)
			}
		}
		if len(r.Reason) > 10000 {
			t.Errorf("the reason is %d bytes", len(r.Reason))
		}
		out := string(f.outputs["s"])
		if len(out) < 1<<20 || !strings.HasPrefix(out, "=== RUN   TestEarly\n") || !strings.HasSuffix(out, "FAIL\tgithub.com/o/r\t0.4s\n") || strings.Contains(out, "bytes of output cut") {
			t.Errorf("kept %d bytes, want the whole output", len(out))
		}
	})
	t.Run("a gate output that cannot be kept still fails with its reason", func(t *testing.T) {
		f := newFixture(t, "echo error: broke; exit 1")
		f.refuseOutputs = true
		r := f.e.run(ctx, f.claim("gate", map[string]any{}, f.chain())).report()
		if r.Outcome != "failed" || r.Retryable || !strings.Contains(r.Reason, "error: broke") || !strings.Contains(r.Reason, "could not be kept") {
			t.Errorf("gate = %+v", r)
		}
	})
	t.Run("missing config is final at workspace", func(t *testing.T) {
		f := newFixture(t, "true")
		f.origin = newOrigin(t, map[string]string{"README.md": "x"})
		f.e.cfg.GitURL = "file://" + f.origin
		r := f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil)).report()
		if r.Outcome != "failed" || r.Retryable || !strings.Contains(r.Reason, ".factory/config.toml") {
			t.Errorf("workspace = %+v", r)
		}
	})
	t.Run("a crashed session is retryable", func(t *testing.T) {
		f := newFixture(t, "true")
		ws := mustSucceed(t, f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil)))
		f.agent.RunFunc = func(context.Context, harness.Request) error { return errors.New("exit status 1") }
		r := f.e.run(ctx, f.claim("implement", map[string]any{}, map[string]any{testRepo + "/workspace": ws.Result})).report()
		if r.Outcome != "failed" || !r.Retryable {
			t.Errorf("implement = %+v", r)
		}
	})
	t.Run("a question blocks", func(t *testing.T) {
		f := newFixture(t, "true")
		ws := mustSucceed(t, f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil)))
		f.agent.RunFunc = writes(map[string]string{blockedPath: "Which API version?"})
		r := f.e.run(ctx, f.claim("implement", map[string]any{}, map[string]any{testRepo + "/workspace": ws.Result})).report()
		if r.Outcome != "blocked" || r.Reason != "Which API version?" {
			t.Errorf("implement = %+v", r)
		}
	})
	t.Run("a secret refuses the push", func(t *testing.T) {
		f := newFixture(t, "true")
		ws := mustSucceed(t, f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil)))
		f.agent.RunFunc = writes(map[string]string{"leak.txt": "token " + testToken})
		r := f.e.run(ctx, f.claim("implement", map[string]any{}, map[string]any{testRepo + "/workspace": ws.Result})).report()
		if r.Outcome != "failed" || r.Retryable || !strings.Contains(r.Reason, "leak.txt:1") || strings.Contains(r.Reason, testToken) {
			t.Errorf("implement = %+v", r)
		}
	})
	t.Run("usage limit moves to the next harness", func(t *testing.T) {
		f := newFixture(t, "true")
		ws := mustSucceed(t, f.e.run(ctx, f.claim("workspace", map[string]any{"branch": testBranch}, nil)))
		limited := &harness.Fake{HarnessName: "first", RunFunc: func(context.Context, harness.Request) error {
			return &harness.UsageLimitError{Harness: "first", Tail: "halfway"}
		}}
		f.e.cfg.Harnesses = []harness.Harness{limited, f.agent}
		f.agent.RunFunc = writes(map[string]string{"hello.txt": "hi"})
		mustSucceed(t, f.e.run(ctx, f.claim("implement", map[string]any{}, map[string]any{testRepo + "/workspace": ws.Result})))
		if !strings.Contains(f.agent.Requests[0].Prompt, "halfway") {
			t.Error("the next harness was not told how far the first got")
		}
		f.agent.RunFunc = func(context.Context, harness.Request) error { return &harness.UsageLimitError{Harness: "fake"} }
		r := f.e.run(ctx, f.claim("review", map[string]any{}, map[string]any{testRepo + "/workspace": ws.Result})).report()
		if r.Outcome != "failed" || !r.Retryable || !strings.Contains(r.Reason, "usage limit") {
			t.Errorf("review with every harness exhausted = %+v", r)
		}
	})
}

func TestFixAppliesFindingsOnce(t *testing.T) {
	f := newFixture(t, "test -f fixed.txt")
	results := f.chain()
	results[testRepo+"/review-1"] = reviewResult{Verdict: "changes", Findings: "add fixed.txt"}
	f.agent.RunFunc = writes(map[string]string{"fixed.txt": "ok"})
	ctx := t.Context()
	params := map[string]any{"round": 1, "findings": testRepo + "/review-1"}
	r := mustSucceed(t, f.e.run(ctx, f.claim("fix", params, results)))
	if !strings.Contains(f.agent.Requests[len(f.agent.Requests)-1].Prompt, "add fixed.txt") {
		t.Error("the fix prompt does not carry the findings")
	}
	bare := filepath.Join(f.origin, testRepo+".git")
	if git(t, bare, "rev-parse", testBranch) != asMap(t, r.Result)["head"] {
		t.Error("the fix was not pushed")
	}
	if msg := git(t, bare, "log", "-1", "--format=%B", testBranch); msg != "Address the findings of "+testRepo+"/review-1\n\n"+wantSignOff {
		t.Errorf("fix commit message = %q, want it signed off", msg)
	}
	sessions := len(f.agent.Requests)
	f.e.cfg.WorkDir = t.TempDir()
	mustSucceed(t, f.e.run(ctx, f.claim("fix", params, results)))
	if len(f.agent.Requests) != sessions {
		t.Error("a retried fix ran the session again over a pushed fix")
	}
	if r := f.e.run(ctx, f.claim("fix", map[string]any{"round": 2, "findings": testRepo + "/review-2"}, results)).report(); r.Outcome != "failed" || r.Retryable {
		t.Errorf("fix without findings = %+v", r)
	}
	results[testRepo+"/await_checks-1"] = map[string]any{"verdict": "fail", "failed_checks": []string{"lint"}}
	f.agent.RunFunc = writes(map[string]string{"fixed.txt": "lint fixed"})
	mustSucceed(t, f.e.run(ctx, f.claim("fix", map[string]any{"round": 1, "findings": testRepo + "/await_checks-1"}, results)))
	if prompt := f.agent.Requests[len(f.agent.Requests)-1].Prompt; !strings.Contains(prompt, `["lint"]`) {
		t.Errorf("the checks fix prompt does not name the failed checks: %q", prompt)
	}
}

// TestFixRepairsItsGate hands a fix session whose gate fails that gate's
// whole output, once, and pushes what makes the gate pass.
func TestFixRepairsItsGate(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t, "test -f repaired.txt || { echo 'error: repaired.txt is missing'; exit 1; }")
	results := f.chain()
	results[testRepo+"/review-1"] = reviewResult{Verdict: "changes", Findings: "add fixed.txt"}
	params := map[string]any{"round": 1, "findings": testRepo + "/review-1"}
	var handed string
	f.agent.RunFunc = func(c context.Context, req harness.Request) error {
		if !strings.Contains(req.Prompt, "the repository's gate command failed") {
			return writes(map[string]string{"fixed.txt": "ok"})(c, req)
		}
		data, err := os.ReadFile(filepath.Join(req.Dir, filepath.FromSlash(gateOutputPath)))
		if err != nil {
			return err
		}
		handed = string(data)
		return writes(map[string]string{"repaired.txt": "ok"})(c, req)
	}
	r := mustSucceed(t, f.e.run(ctx, f.claim("fix", params, results)))
	if len(f.agent.Requests) != 3 {
		t.Fatalf("%d sessions, want implement, fix and the repair", len(f.agent.Requests))
	}
	if prompt := f.agent.Requests[2].Prompt; !strings.Contains(prompt, "error: repaired.txt is missing") || !strings.Contains(prompt, gateOutputPath) {
		t.Errorf("the repair prompt does not carry the failure:\n%s", prompt)
	}
	if handed != "error: repaired.txt is missing\n" {
		t.Errorf("the session was handed %q", handed)
	}
	bare := filepath.Join(f.origin, testRepo+".git")
	if git(t, bare, "rev-parse", testBranch) != asMap(t, r.Result)["head"] {
		t.Error("the repair was not pushed")
	}
	if files := git(t, bare, "ls-tree", "-r", "--name-only", testBranch); !strings.Contains(files, "repaired.txt") || strings.Contains(files, "gate-output") {
		t.Errorf("the branch holds:\n%s", files)
	}
	if string(f.outputs["s"]) != "" {
		t.Errorf("the kept output is %q, want the passing run's", f.outputs["s"])
	}

	t.Run("a second failure is final", func(t *testing.T) {
		f := newFixture(t, "echo 'error: still broken'; exit 1")
		results := f.chain()
		results[testRepo+"/review-1"] = reviewResult{Verdict: "changes", Findings: "add fixed.txt"}
		f.agent.RunFunc = writes(map[string]string{"fixed.txt": "ok"})
		r := f.e.run(ctx, f.claim("fix", params, results)).report()
		if r.Outcome != "failed" || r.Retryable || !strings.Contains(r.Reason, "error: still broken") || len(f.agent.Requests) != 3 {
			t.Errorf("fix = %+v after %d sessions", r, len(f.agent.Requests))
		}
	})
	t.Run("a repair that changes nothing fails with the gate's reason", func(t *testing.T) {
		f := newFixture(t, "test -f never.txt || { echo 'error: never'; exit 1; }")
		results := f.chain()
		results[testRepo+"/review-1"] = reviewResult{Verdict: "changes", Findings: "add fixed.txt"}
		f.agent.RunFunc = func(c context.Context, req harness.Request) error {
			if strings.Contains(req.Prompt, "the repository's gate command failed") {
				return nil
			}
			return writes(map[string]string{"fixed.txt": "ok"})(c, req)
		}
		r := f.e.run(ctx, f.claim("fix", params, results)).report()
		if r.Outcome != "failed" || r.Retryable || !strings.Contains(r.Reason, "error: never") {
			t.Errorf("fix = %+v", r)
		}
	})
}

func TestMerge(t *testing.T) {
	ctx := t.Context()
	f := newFixture(t, "true")
	f.host.Pulls = []codehost.Pull{{Number: 1, HTMLURL: "https://github.test/o/r/pull/1", State: "open"}}
	f.host.Pulls[0].Head.Ref = testBranch
	open := Output{Repo: testRepo, Kind: "pull_request", Branch: testBranch, URL: "https://github.test/o/r/pull/1", Status: "open"}
	r := mustSucceed(t, f.e.run(ctx, f.claim("merge", map[string]any{}, nil, open)))
	if got := f.host.Merged(); len(got) != 1 || got[0] != 1 || len(r.Outputs) != 1 || r.Outputs[0].Status != "merged" || asMap(t, r.Result)["merged"] != true {
		t.Errorf("merge = %+v, merged %v", r, got)
	}
	// Merged on the code host but not yet recorded: recorded, not merged again.
	mustSucceed(t, f.e.run(ctx, f.claim("merge", map[string]any{}, nil, open)))
	if got := f.host.Merged(); len(got) != 1 {
		t.Errorf("merged again: %v", got)
	}
	f.host.Pulls[0].MergedAt, f.host.Pulls[0].State, f.host.QueueID = "", "open", "PR_1"
	r = mustSucceed(t, f.e.run(ctx, f.claim("merge", map[string]any{}, nil, open)))
	if got := f.host.Enqueued(); len(got) != 1 || asMap(t, r.Result)["queued"] != true || len(r.Outputs) != 0 {
		t.Errorf("queue = %+v, enqueued %v", r, got)
	}
	if r := f.e.run(ctx, f.claim("merge", map[string]any{}, nil)).report(); r.Outcome != "failed" || r.Retryable {
		t.Errorf("merge without a pull request = %+v", r)
	}
}
