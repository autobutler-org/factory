package codehost

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/codehost/fakegh"
)

// statusReply is a GraphQL answer with branch protection, a ruleset, two
// reviews, and both kinds of check: check runs and commit statuses.
const statusReply = `{"data":{"repository":{"pullRequest":{
  "state":"OPEN","reviewDecision":null,
  "latestOpinionatedReviews":{"nodes":[{"state":"APPROVED"},{"state":"COMMENTED"}]},
  "baseRef":{
    "branchProtectionRule":{"requiredStatusCheckContexts":["ci","lint"]},
    "rules":{"nodes":[
      {"type":"REQUIRED_STATUS_CHECKS","parameters":{"requiredStatusChecks":[{"context":"lint"},{"context":"deploy/preview"}]}},
      {"type":"PULL_REQUEST","parameters":{}}
    ]}
  },
  "commits":{"nodes":[{"commit":{"oid":"abc123","committedDate":"2026-11-20T10:00:00Z","statusCheckRollup":{"contexts":{
    "pageInfo":{"hasNextPage":false},
    "nodes":[
      {"__typename":"CheckRun","name":"ci","status":"COMPLETED","conclusion":"SUCCESS"},
      {"__typename":"CheckRun","name":"lint","status":"IN_PROGRESS","conclusion":null},
      {"__typename":"CheckRun","name":"docs","status":"COMPLETED","conclusion":"SKIPPED"},
      {"__typename":"CheckRun","name":"race","status":"COMPLETED","conclusion":"TIMED_OUT"},
      {"__typename":"StatusContext","context":"deploy/preview","state":"PENDING"},
      {"__typename":"StatusContext","context":"license","state":"ERROR"},
      {"__typename":"StatusContext","context":"cla","state":"SUCCESS"}
    ]}}}}]}
}}}}`

func TestPullStatusReadsOneGraphQLAnswer(t *testing.T) {
	var asked struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/graphql" {
			t.Errorf("path = %s, want /graphql", r.URL.Path)
		}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &asked); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(statusReply))
	}))
	t.Cleanup(srv.Close)
	got, err := (GitHub{API: srv.URL, Repo: testRepo, Token: testToken}).PullStatus(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || asked.Variables["number"] != float64(7) || asked.Variables["owner"] != "o" || asked.Variables["name"] != "r" {
		t.Errorf("%d calls, variables %v; want one call for o/r#7", calls, asked.Variables)
	}
	for _, field := range []string{"statusCheckRollup", "branchProtectionRule", "rules(", "reviewDecision", "StatusContext", "CheckRun"} {
		if !strings.Contains(asked.Query, field) {
			t.Errorf("query does not ask for %s", field)
		}
	}
	want := PullStatus{
		State: PullOpen, Head: "abc123", HeadCommitted: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), Approved: true,
		Required: []string{"ci", "lint", "deploy/preview"},
		Checks: []Check{
			{"ci", CheckPassed},
			{"lint", CheckPending},
			{"docs", CheckPassed},
			{"race", CheckFailed},
			{"deploy/preview", CheckPending},
			{"license", CheckFailed},
			{"cla", CheckPassed},
		},
	}
	if got.State != want.State || got.Head != want.Head || !got.HeadCommitted.Equal(want.HeadCommitted) || got.Approved != want.Approved ||
		!slices.Equal(got.Required, want.Required) || !slices.Equal(got.Checks, want.Checks) {
		t.Errorf("PullStatus =\n%+v\nwant\n%+v", got, want)
	}
}

func TestPullStatusStates(t *testing.T) {
	for _, tt := range []struct{ github, want string }{
		{"OPEN", PullOpen}, {"CLOSED", PullClosed}, {"MERGED", PullMerged},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{"state":"` + tt.github + `","baseRef":null,"commits":{"nodes":[]}}}}}`))
		}))
		got, err := (GitHub{API: srv.URL, Repo: testRepo, Token: testToken}).PullStatus(t.Context(), 1)
		srv.Close()
		if err != nil || got.State != tt.want || got.Head != "" || len(got.Checks) != 0 {
			t.Errorf("%s: PullStatus = %+v, %v; want %s with no head", tt.github, got, err, tt.want)
		}
	}
}

func TestApproved(t *testing.T) {
	for _, tt := range []struct {
		name     string
		decision string
		reviews  []string
		want     bool
	}{
		{"the repository's rules say approved", "APPROVED", nil, true},
		{"the rules still want a review", "REVIEW_REQUIRED", []string{"APPROVED"}, false},
		{"changes requested", "CHANGES_REQUESTED", []string{"APPROVED"}, false},
		{"no rule, one approval", "", []string{"COMMENTED", "APPROVED"}, true},
		{"no rule, an approval and a change request", "", []string{"APPROVED", "CHANGES_REQUESTED"}, false},
		{"no rule, no review", "", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := approved(tt.decision, tt.reviews); got != tt.want {
				t.Errorf("approved(%q, %v) = %v, want %v", tt.decision, tt.reviews, got, tt.want)
			}
		})
	}
}

func TestPullStatusFromFakegh(t *testing.T) {
	s, g := newGitHub(t)
	p := s.SeedPull(fakegh.Pull{Head: "0f3c2a9e_x", Base: "main"})
	s.SetChecks([]fakegh.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}, {Name: "e2e", Status: "in_progress"}})
	got, err := g.PullStatus(t.Context(), p.Number)
	if err != nil || got.State != PullOpen || got.Head != fakegh.HeadSHA || got.Approved ||
		!slices.Equal(got.Checks, []Check{{"ci", CheckPassed}, {"e2e", CheckPending}}) {
		t.Fatalf("PullStatus = %+v, %v; want open at HeadSHA, ci passed and e2e pending", got, err)
	}
	s.Approve(p.Number)
	if got, err := g.PullStatus(t.Context(), p.Number); err != nil || !got.Approved {
		t.Errorf("PullStatus after an approval = %+v, %v", got, err)
	}
	if err := g.MergePull(t.Context(), p.Number); err != nil {
		t.Fatal(err)
	}
	if got, err := g.PullStatus(t.Context(), p.Number); err != nil || got.State != PullMerged {
		t.Errorf("PullStatus after the merge = %+v, %v", got, err)
	}
	s.ClosePull(s.SeedPull(fakegh.Pull{Head: "0f3c2a9e_y", Base: "main"}).Number)
	if got, err := g.PullStatus(t.Context(), 2); err != nil || got.State != PullClosed {
		t.Errorf("PullStatus of a closed pull request = %+v, %v", got, err)
	}
	// The merge queue's reads get the same answer and still parse.
	if id, err := g.QueuedPull(t.Context(), 1); err != nil || id != "" {
		t.Errorf("QueuedPull = %q, %v", id, err)
	}
}

func TestFakePullStatus(t *testing.T) {
	f := &Fake{
		HeadChecks: map[string][]Check{"new": {{"ci", CheckPending}}, "old": {{"ci", CheckFailed}}},
		Required:   []string{"ci"},
		Approved:   []int{1},
	}
	if _, err := f.OpenPull(t.Context(), "t", "", "0f3c2a9e_x", "main"); err != nil {
		t.Fatal(err)
	}
	f.Pulls[0].Head.Sha = "new"
	got, err := f.PullStatus(t.Context(), 1)
	if err != nil || got.State != PullOpen || got.Head != "new" || !got.Approved ||
		!slices.Equal(got.Checks, []Check{{"ci", CheckPending}}) || !slices.Equal(got.Required, []string{"ci"}) {
		t.Fatalf("PullStatus = %+v, %v; want the new head's checks only", got, err)
	}
	if _, err := f.PullStatus(t.Context(), 2); err == nil {
		t.Error("PullStatus of a pull request that does not exist: no error")
	}
	f.Pulls[0].State = "closed"
	if got, _ := f.PullStatus(t.Context(), 1); got.State != PullClosed {
		t.Errorf("closed pull = %s", got.State)
	}
	if err := f.MergePull(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.PullStatus(t.Context(), 1); got.State != PullMerged {
		t.Errorf("merged pull = %s", got.State)
	}
}
