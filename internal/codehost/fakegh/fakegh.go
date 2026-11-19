// Package fakegh is a stateful fake of the GitHub REST endpoints a worker
// calls for one repository: the repository, pulls (list, create, retarget,
// merge), pull request comments, check runs and their Actions job logs,
// branch protection, a branch's head commit and file contents, token revocation, and a fixed GraphQL answer for the
// merge queue. List endpoints honor per_page and page. Merge can be told to
// fail the way GitHub does. Accept adds tokens that expire, and an expired
// token gets GitHub's 401 Bad credentials.
//
// It is for tests only and never talks to GitHub.
package fakegh

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Pull is a pull request as the fake stores it.
type Pull struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	URL    string `json:"html_url"`
	// State is "open" or "closed". Empty is open.
	State  string `json:"state,omitempty"`
	Merged bool   `json:"merged,omitempty"`
}

// CheckRun is one check run on a commit.
type CheckRun struct {
	// ID is also the Actions job ID whose log SetLog sets.
	ID         int64  `json:"id,omitempty"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// mergeErrors are GitHub's messages for the merge failures it documents.
var mergeErrors = map[int]string{
	http.StatusMethodNotAllowed:    "Pull Request is not mergeable",
	http.StatusConflict:            "Head branch was modified. Review and try the merge again.",
	http.StatusUnprocessableEntity: "Validation Failed",
}

// HeadSHA is the commit every branch of the fake points at.
const HeadSHA = "4ead4ead4ead4ead4ead4ead4ead4ead4ead4ead"

const mergeDocs = "https://docs.github.com/rest/pulls/pulls#merge-a-pull-request"

// Server is the fake. Start one with New.
type Server struct {
	// URL is the API base URL to hand to the client.
	URL string

	t           testing.TB
	owner, repo string

	mu sync.Mutex
	// tokens maps each accepted token to when it expires. A zero time
	// never expires.
	tokens   map[string]time.Time
	pulls    []Pull
	comments map[int][]string // by pull request number
	calls    []string
	checks   []CheckRun
	logs     map[int64]string // by job ID
	// merges is how many merge calls landed, and mergeMethod is the last one.
	merges      int
	mergeMethod string
	// mergeStatus, when set, is the error status every merge call returns.
	mergeStatus int
	// approvals is the required approving review count by protected branch.
	approvals map[string]int
	// revoked is how many DELETE /installation/token calls landed. The fake
	// keeps accepting the token afterward.
	revoked int
	// revokeStatus, when set, is the error status every revoke call returns.
	revokeStatus int
	// files are the repository's files by path, at HeadSHA.
	files map[string]string
}

// New starts a fake for repo ("owner/name") that accepts only token. It is
// closed when the test ends.
func New(t testing.TB, repo, token string) *Server {
	owner, _, _ := strings.Cut(repo, "/")
	s := &Server{t: t, owner: owner, repo: repo, tokens: map[string]time.Time{token: {}}, comments: map[int][]string{}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Accept makes the fake also accept token until expires, after which a
// request with it gets 401 Bad credentials, as GitHub answers an expired
// installation token.
func (s *Server) Accept(token string, expires time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[token] = expires
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, docs string) {
	writeJSON(w, status, map[string]string{"message": message, "documentation_url": docs})
}

// page returns the slice of items that per_page and page select. GitHub's
// defaults are 30 per page and page 1.
func page[T any](r *http.Request, items []T) []T {
	per, err := strconv.Atoi(r.URL.Query().Get("per_page"))
	if err != nil || per < 1 {
		per = 30
	}
	n, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || n < 1 {
		n = 1
	}
	start := min((n-1)*per, len(items))
	return append([]T{}, items[start:min(start+per, len(items))]...)
}

func (p Pull) reply() map[string]any {
	var mergedAt any
	if p.Merged {
		mergedAt = time.Date(2026, 1, 2, 0, 0, p.Number, 0, time.UTC).Format(time.RFC3339)
	}
	return map[string]any{
		"number":     p.Number,
		"title":      p.Title,
		"body":       p.Body,
		"html_url":   p.URL,
		"state":      orDefault(p.State, "open"),
		"merged":     p.Merged,
		"merged_at":  mergedAt,
		"created_at": time.Date(2026, 1, 1, 0, 0, p.Number, 0, time.UTC).Format(time.RFC3339),
		"head":       map[string]string{"ref": p.Head},
		"base":       map[string]string{"ref": p.Base},
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	got := r.Header.Get("Authorization")
	token, bearer := strings.CutPrefix(got, "Bearer ")
	expires, known := s.tokens[token]
	known = known && bearer
	if !known {
		s.t.Errorf("%s %s: Authorization = %q, want the worker's token", r.Method, r.URL, got)
	}
	if !known || (!expires.IsZero() && !time.Now().Before(expires)) {
		s.calls = append(s.calls, "401 "+r.Method+" "+r.URL.Path)
		writeError(w, http.StatusUnauthorized, "Bad credentials", "https://docs.github.com/rest")
		return
	}
	s.calls = append(s.calls, r.Method+" "+r.URL.Path)
	if r.Method == http.MethodDelete && r.URL.Path == "/installation/token" {
		if s.revokeStatus != 0 {
			writeError(w, s.revokeStatus, http.StatusText(s.revokeStatus), "https://docs.github.com/rest/apps/installations#revoke-an-installation-access-token")
			return
		}
		s.revoked++
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
		// ponytail: every GraphQL query gets one answer, a pull request on
		// main whose branch has no merge queue.
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"repository": map[string]any{
			"pullRequest": map[string]string{"id": "PR_1", "baseRefName": "main"},
			"mergeQueue":  nil,
		}}})
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/repos/"+s.repo)
	if !ok {
		s.unexpected(w, r)
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case r.Method == http.MethodGet && path == "":
		writeJSON(w, http.StatusOK, map[string]any{"full_name": s.repo, "default_branch": "main"})
	case r.Method == http.MethodGet && path == "/pulls":
		s.listPulls(w, r)
	case r.Method == http.MethodPost && path == "/pulls":
		s.createPull(w, r)
	case r.Method == http.MethodPatch && len(parts) == 2 && parts[0] == "pulls":
		s.retarget(w, r, parts[1])
	case r.Method == http.MethodPut && len(parts) == 3 && parts[0] == "pulls" && parts[2] == "merge":
		s.merge(w, r, parts[1])
	case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "issues" && parts[2] == "comments":
		s.comment(w, r, parts[1])
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/branches/") && strings.HasSuffix(path, "/protection"):
		branch := strings.TrimSuffix(strings.TrimPrefix(path, "/branches/"), "/protection")
		n, ok := s.approvals[branch]
		if !ok {
			writeError(w, http.StatusNotFound, "Branch not protected", "https://docs.github.com/rest/branches/branch-protection#get-branch-protection")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"required_pull_request_reviews": map[string]int{"required_approving_review_count": n}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/branches/"):
		writeJSON(w, http.StatusOK, map[string]any{"name": strings.TrimPrefix(path, "/branches/"), "commit": map[string]string{"sha": HeadSHA}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/contents/"):
		content, ok := s.files[strings.TrimPrefix(path, "/contents/")]
		if ref := r.URL.Query().Get("ref"); !ok || (ref != "" && ref != HeadSHA && ref != "main") {
			writeError(w, http.StatusNotFound, "Not Found", "https://docs.github.com/rest/repos/contents#get-repository-content")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type": "file", "encoding": "base64", "path": strings.TrimPrefix(path, "/contents/"),
			"content": base64.StdEncoding.EncodeToString([]byte(content)),
		})
	case r.Method == http.MethodGet && len(parts) == 3 && parts[0] == "commits" && parts[2] == "check-runs":
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(s.checks), "check_runs": page(r, s.checks)})
	case r.Method == http.MethodGet && len(parts) == 4 && parts[0] == "actions" && parts[1] == "jobs" && parts[3] == "logs":
		id, err := strconv.ParseInt(parts[2], 10, 64)
		log, ok := s.logs[id]
		if err != nil || !ok {
			writeError(w, http.StatusNotFound, "Not Found", "https://docs.github.com/rest/actions/workflow-jobs#download-job-logs-for-a-workflow-run")
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(log))
	default:
		s.unexpected(w, r)
	}
}

func (s *Server) unexpected(w http.ResponseWriter, r *http.Request) {
	s.t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL)
	writeError(w, http.StatusNotFound, "Not Found", "https://docs.github.com/rest")
}

// listPulls filters by state (open by default, as GitHub does) and by head
// ("owner:branch"), newest first.
func (s *Server) listPulls(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state, head := q.Get("state"), q.Get("head")
	if state == "" {
		state = "open"
	}
	found := []map[string]any{}
	for _, p := range slices.Backward(s.pulls) {
		if head != "" && head != s.owner+":"+p.Head {
			continue
		}
		if state != "all" && state != orDefault(p.State, "open") {
			continue
		}
		found = append(found, p.reply())
	}
	writeJSON(w, http.StatusOK, page(r, found))
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (s *Server) createPull(w http.ResponseWriter, r *http.Request) {
	var p Pull
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Title == "" || p.Head == "" || p.Base == "" {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed", "https://docs.github.com/rest/pulls/pulls#create-a-pull-request")
		return
	}
	writeJSON(w, http.StatusCreated, s.add(p).reply())
}

// add numbers p and stores it. It holds s.mu.
func (s *Server) add(p Pull) Pull {
	p.Number = len(s.pulls) + 1
	p.State = orDefault(p.State, "open")
	if p.URL == "" {
		p.URL = fmt.Sprintf("https://github.test/%s/pull/%d", s.repo, p.Number)
	}
	s.pulls = append(s.pulls, p)
	return p
}

// pull returns the stored pull request for a path number, or nil.
func (s *Server) pull(number string) *Pull {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 || n > len(s.pulls) {
		return nil
	}
	return &s.pulls[n-1]
}

// retarget handles PATCH /pulls/{n} with a new base, the only edit a worker
// makes.
func (s *Server) retarget(w http.ResponseWriter, r *http.Request, number string) {
	const docs = "https://docs.github.com/rest/pulls/pulls#update-a-pull-request"
	p := s.pull(number)
	if p == nil {
		writeError(w, http.StatusNotFound, "Not Found", docs)
		return
	}
	var body struct {
		Base string `json:"base"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Base == "" {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed", docs)
		return
	}
	p.Base = body.Base
	writeJSON(w, http.StatusOK, p.reply())
}

func (s *Server) comment(w http.ResponseWriter, r *http.Request, number string) {
	const docs = "https://docs.github.com/rest/issues/comments#create-an-issue-comment"
	p := s.pull(number)
	if p == nil {
		writeError(w, http.StatusNotFound, "Not Found", docs)
		return
	}
	var c struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil || c.Body == "" {
		writeError(w, http.StatusUnprocessableEntity, "Validation Failed", docs)
		return
	}
	s.comments[p.Number] = append(s.comments[p.Number], c.Body)
	writeJSON(w, http.StatusCreated, map[string]any{"id": len(s.comments[p.Number]), "body": c.Body, "user": map[string]string{"type": "Bot"}})
}

func (s *Server) merge(w http.ResponseWriter, r *http.Request, number string) {
	p := s.pull(number)
	if p == nil {
		writeError(w, http.StatusNotFound, "Not Found", mergeDocs)
		return
	}
	var body struct {
		Method string `json:"merge_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Problems parsing JSON", mergeDocs)
		return
	}
	status := s.mergeStatus
	if status == 0 && p.State != "open" {
		status = http.StatusMethodNotAllowed
	}
	if status != 0 {
		writeError(w, status, mergeErrors[status], mergeDocs)
		return
	}
	s.merges++
	s.mergeMethod = body.Method
	p.State, p.Merged = "closed", true
	writeJSON(w, http.StatusOK, map[string]any{"sha": fmt.Sprintf("%040d", p.Number), "merged": true, "message": "Pull Request successfully merged"})
}

// FailMerge makes every later merge call return status (405, 409, or 422)
// with GitHub's JSON error body for it.
func (s *Server) FailMerge(status int) {
	s.t.Helper()
	if _, ok := mergeErrors[status]; !ok {
		s.t.Fatalf("FailMerge(%d): GitHub's merge fails with 405, 409, or 422", status)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeStatus = status
}

// AllowMerge lets a later merge call land after FailMerge.
func (s *Server) AllowMerge() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mergeStatus = 0
}

// FailRevoke makes every DELETE /installation/token call return status, and
// leaves the token usable. Zero lets revokes land again.
func (s *Server) FailRevoke(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeStatus = status
}

// Revoked reports how many times the token was revoked.
func (s *Server) Revoked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revoked
}

// Protect makes branch protected, requiring approvals approving reviews.
// An unprotected branch answers 404, as GitHub does.
func (s *Server) Protect(branch string, approvals int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.approvals == nil {
		s.approvals = map[string]int{}
	}
	s.approvals[branch] = approvals
}

// SeedPull stores p as if it had been opened, and returns it numbered.
func (s *Server) SeedPull(p Pull) Pull {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(p)
}

// SetChecks sets the check runs every commit has.
func (s *Server) SetChecks(runs []CheckRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = slices.Clone(runs)
}

// SetFile puts a file with content at path on every branch.
func (s *Server) SetFile(path, content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.files == nil {
		s.files = map[string]string{}
	}
	s.files[path] = content
}

// SetLog sets the log of Actions job id. A job without one is not found.
func (s *Server) SetLog(id int64, log string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.logs == nil {
		s.logs = map[int64]string{}
	}
	s.logs[id] = log
}

// Snapshot returns the pull requests and the comments by pull request number.
func (s *Server) Snapshot() ([]Pull, map[int][]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	comments := map[int][]string{}
	for k, cs := range s.comments {
		comments[k] = slices.Clone(cs)
	}
	return slices.Clone(s.pulls), comments
}

// Calls returns every call so far as "METHOD /path". A call refused for
// its token is "401 METHOD /path".
func (s *Server) Calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// Merges returns how many merges landed and the last merge method.
func (s *Server) Merges() (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.merges, s.mergeMethod
}
