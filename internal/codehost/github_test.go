package codehost

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/codehost/fakegh"
)

const (
	testRepo  = "o/r"
	testToken = "worker-token"
)

func newGitHub(t *testing.T) (*fakegh.Server, GitHub) {
	t.Helper()
	s := fakegh.New(t, testRepo, testToken)
	return s, GitHub{API: s.URL, Repo: testRepo, Token: testToken}
}

// TestOpenFindCommentRetargetMerge runs one pull request through every REST
// write a worker makes.
func TestOpenFindCommentRetargetMerge(t *testing.T) {
	s, g := newGitHub(t)
	ctx := t.Context()
	if got, err := g.DefaultBranch(ctx); err != nil || got != "main" {
		t.Fatalf("DefaultBranch = %q, %v", got, err)
	}
	if _, ok, err := g.FindPull(ctx, "0f3c2a9e_x", "all"); err != nil || ok {
		t.Fatalf("FindPull before opening = %v, %v; want none", ok, err)
	}
	opened, err := g.OpenPull(ctx, "add x", "body", "0f3c2a9e_x", "aaaaaaaa_below")
	if err != nil {
		t.Fatal(err)
	}
	if opened.Number != 1 || opened.HTMLURL != "https://github.test/o/r/pull/1" || opened.Head.Ref != "0f3c2a9e_x" {
		t.Errorf("OpenPull = %+v", opened)
	}
	// Another branch that merely shares a prefix is not a match.
	s.SeedPull(fakegh.Pull{Title: "other", Head: "0f3c2a9e_x2", Base: "main"})
	found, ok, err := g.FindPull(ctx, "0f3c2a9e_x", "open")
	if err != nil || !ok || found.Number != 1 {
		t.Fatalf("FindPull = %+v, %v, %v; want pull 1", found, ok, err)
	}
	if err := g.Comment(ctx, 1, "see also"); err != nil {
		t.Fatal(err)
	}
	if err := g.RetargetPull(ctx, 1, "main"); err != nil {
		t.Fatal(err)
	}
	if err := g.UpdatePull(ctx, 1, "add x, fixed", "new body"); err != nil {
		t.Fatal(err)
	}
	if err := g.MergePull(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := g.MergePull(ctx, 1); err == nil {
		t.Error("merging a merged pull request: no error")
	}
	pulls, comments := s.Snapshot()
	if pulls[0].Base != "main" || !pulls[0].Merged || pulls[0].Title != "add x, fixed" || pulls[0].Body != "new body" {
		t.Errorf("pull 1 = %+v, want retargeted to main, its text updated, and merged", pulls[0])
	}
	if !slices.Equal(comments[1], []string{"see also"}) {
		t.Errorf("comments = %v", comments)
	}
	if n, method := s.Merges(); n != 1 || method != "merge" {
		t.Errorf("merges = %d %q, want one merge commit", n, method)
	}
	if found, ok, _ := g.FindPull(ctx, "0f3c2a9e_x", "closed"); !ok || found.MergedAt == "" {
		t.Errorf("FindPull closed = %+v, %v; want merged", found, ok)
	}
}

func TestCheckRunsAndJobLog(t *testing.T) {
	s, g := newGitHub(t)
	s.SetChecks([]fakegh.CheckRun{{ID: 9, Name: "ci", Status: "completed", Conclusion: "failure"}})
	s.SetLog(9, "FAIL: TestThing\n")
	runs, err := g.CheckRuns(t.Context(), "abc")
	if err != nil || len(runs) != 1 || runs[0] != (CheckRun{ID: 9, Name: "ci", Status: "completed", Conclusion: "failure"}) {
		t.Fatalf("CheckRuns = %+v, %v", runs, err)
	}
	if log, err := g.JobLog(t.Context(), runs[0].ID); err != nil || string(log) != "FAIL: TestThing\n" {
		t.Errorf("JobLog = %q, %v", log, err)
	}
}

func TestMergeQueue(t *testing.T) {
	_, g := newGitHub(t)
	if id, err := g.QueuedPull(t.Context(), 1); err != nil || id != "" {
		t.Errorf("QueuedPull = %q, %v; want no queue", id, err)
	}
	if err := g.EnqueuePull(t.Context(), "PR_1"); err != nil {
		t.Errorf("EnqueuePull = %v", err)
	}
}

func TestRevokeToken(t *testing.T) {
	s, g := newGitHub(t)
	if err := g.RevokeToken(t.Context(), testToken); err != nil || s.Revoked() != 1 {
		t.Fatalf("RevokeToken = %v, revoked %d", err, s.Revoked())
	}
	s.FailRevoke(http.StatusBadGateway)
	if err := g.RevokeToken(t.Context(), testToken); err == nil {
		t.Error("RevokeToken on a 502: no error")
	}
}

func TestProtectionReadsRequiredReviews(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   string
		want   int
	}{
		{"reviews", http.StatusOK, `{"required_pull_request_reviews":{"required_approving_review_count":1}}`, 1},
		{"status checks only", http.StatusOK, `{"required_status_checks":{"strict":true,"contexts":["ci"]}}`, 0},
		{"not protected", http.StatusNotFound, `{"message":"Branch not protected"}`, 0},
		{"no access", http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/"+testRepo+"/branches/feat/x/protection" {
					t.Errorf("path = %s", r.URL.Path)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)
			got, err := (GitHub{API: srv.URL, Repo: testRepo, Token: testToken}).Protection(t.Context(), "feat/x")
			if err != nil || got.RequiredApprovals != tt.want {
				t.Fatalf("Protection = %+v, %v, want %d approvals", got, err, tt.want)
			}
		})
	}
}

func TestProtectionFromFakegh(t *testing.T) {
	s, g := newGitHub(t)
	s.Protect("release/1", 2)
	if got, err := g.Protection(t.Context(), "release/1"); err != nil || got.RequiredApprovals != 2 {
		t.Errorf("Protection = %+v, %v; want 2 approvals", got, err)
	}
}

// TestTokensIsAskedForEachCall sends whatever Tokens returns at the time of
// each call, and does not call GitHub when Tokens fails.
func TestTokensIsAskedForEachCall(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"default_branch":"main"}`))
	}))
	t.Cleanup(srv.Close)
	tokens := []string{"first", "second"}
	refuse := errors.New("no token")
	g := GitHub{API: srv.URL, Repo: testRepo, Tokens: func(context.Context) (string, error) {
		if len(tokens) == 0 {
			return "", refuse
		}
		next := tokens[0]
		tokens = tokens[1:]
		return next, nil
	}}
	for range 2 {
		if _, err := g.DefaultBranch(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.DefaultBranch(t.Context()); !errors.Is(err, refuse) {
		t.Errorf("DefaultBranch with no token = %v, want the token error", err)
	}
	if want := []string{"Bearer first", "Bearer second"}; !slices.Equal(got, want) {
		t.Errorf("Authorization headers = %q, want %q", got, want)
	}
}

func TestProtectionFailsOnAServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	if _, err := (GitHub{API: srv.URL, Repo: testRepo, Token: testToken}).Protection(t.Context(), "main"); err == nil {
		t.Fatal("Protection on a 502 = nil error, want the failure")
	}
}

// TestJobLogFollowsTheRedirect reads the log text GitHub redirects to.
func TestJobLogFollowsTheRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/" + testRepo + "/actions/jobs/9/logs":
			http.Redirect(w, r, "/log", http.StatusFound)
		case "/log":
			fmt.Fprint(w, "short log")
		default:
			t.Errorf("path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	got, err := (GitHub{API: srv.URL, Repo: testRepo, Token: testToken}).JobLog(t.Context(), 9)
	if err != nil || string(got) != "short log" {
		t.Errorf("JobLog = %q, %v", got, err)
	}
}

// TestReadTailKeepsTheEnd keeps the end of a log longer than the limit. A
// shorter log comes back whole.
func TestReadTailKeepsTheEnd(t *testing.T) {
	long := strings.Repeat("head ", 20) + "FAIL: TestThing"
	for _, tc := range []struct{ body, want string }{
		{"short log", "short log"},
		{long, long[len(long)-16:]},
	} {
		got, err := readTail(strings.NewReader(tc.body), 16)
		if err != nil || string(got) != tc.want {
			t.Errorf("readTail = %q, %v; want %q", got, err, tc.want)
		}
	}
}

// TestReadFile reads a file at the commit the branch points at, and tells a
// missing file from other failures.
func TestReadFile(t *testing.T) {
	s, g := newGitHub(t)
	s.SetFile(".factory/config.toml", "gate = \"make check\"\n")
	f, err := g.ReadFile(t.Context(), "main", ".factory/config.toml")
	if err != nil || string(f.Content) != "gate = \"make check\"\n" || f.Commit != fakegh.HeadSHA {
		t.Fatalf("ReadFile = %+v, %v; want the content at %s", f, err, fakegh.HeadSHA)
	}
	if !slices.Contains(s.Calls(), "GET /repos/"+testRepo+"/contents/.factory/config.toml") {
		t.Errorf("calls = %v, want the contents endpoint", s.Calls())
	}
	if _, err := g.ReadFile(t.Context(), "main", "missing.toml"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("ReadFile of a missing file = %v, want ErrFileNotFound", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	_, err = (GitHub{API: srv.URL, Repo: testRepo, Token: testToken}).ReadFile(t.Context(), "main", ".factory/config.toml")
	if err == nil || errors.Is(err, ErrFileNotFound) {
		t.Errorf("ReadFile on a 502 = %v, want a failure that is not ErrFileNotFound", err)
	}
}

func TestFakeReadFile(t *testing.T) {
	f := &Fake{Files: map[string]string{"a.toml": "x"}}
	got, err := f.ReadFile(t.Context(), "main", "a.toml")
	if err != nil || string(got.Content) != "x" || got.Commit == "" {
		t.Errorf("ReadFile = %+v, %v", got, err)
	}
	if _, err := f.ReadFile(t.Context(), "main", "b.toml"); !errors.Is(err, ErrFileNotFound) {
		t.Errorf("ReadFile of a missing file = %v, want ErrFileNotFound", err)
	}
	f.ReadErr = errors.New("down")
	if _, err := f.ReadFile(t.Context(), "main", "a.toml"); err == nil || errors.Is(err, ErrFileNotFound) {
		t.Errorf("ReadFile with ReadErr = %v", err)
	}
}
