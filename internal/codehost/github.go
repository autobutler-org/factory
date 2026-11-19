package codehost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHub is the slice of the GitHub REST and GraphQL APIs a worker needs,
// for one repository and with that worker's token.
type GitHub struct {
	// API is the REST base URL, such as https://api.github.com.
	API string
	// Repo is "owner/name".
	Repo string
	// Token is the token every call sends when Tokens is nil.
	Token string
	// Tokens, when set, is asked for the token before each call, so work
	// that outlives one installation token sends a fresh one. See
	// RefreshingToken.
	Tokens func(context.Context) (string, error)
}

var _ Host = GitHub{}

// statusError is a reply outside 2xx.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

// Bearer is the Authorization header value for one call: Tokens' answer,
// or Token when Tokens is nil.
func (g GitHub) Bearer(ctx context.Context) (string, error) {
	if g.Tokens == nil {
		return "Bearer " + g.Token, nil
	}
	token, err := g.Tokens(ctx)
	if err != nil {
		return "", err
	}
	return "Bearer " + token, nil
}

// do calls one endpoint under /repos/<repo>.
func (g GitHub) do(ctx context.Context, method, path string, in, out any) error {
	data, err := g.send(ctx, method, path, in)
	if err != nil || out == nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// send calls one endpoint under /repos/<repo> and returns the body of a 2xx
// reply. A redirect is followed.
func (g GitHub) send(ctx context.Context, method, path string, in any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(data)
	}
	auth, err := g.Bearer(ctx)
	if err != nil {
		return nil, fmt.Errorf("github %s %s: %w", method, path, err)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(g.API, "/")+"/repos/"+g.Repo+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("github %s %s: %w", method, path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &statusError{code: resp.StatusCode, msg: fmt.Sprintf("github %s %s: %s: %s", method, path, resp.Status, data)}
	}
	return data, nil
}

func (g GitHub) DefaultBranch(ctx context.Context) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	return repo.DefaultBranch, g.do(ctx, http.MethodGet, "", nil, &repo)
}

// pageSize is how many items one list page holds. A shorter page is the
// last page.
const pageSize = 100

// FindPull filters the list by head ("owner:branch"), which GitHub matches
// exactly, so one page holds every pull request from that branch, newest
// first.
func (g GitHub) FindPull(ctx context.Context, branch, state string) (Pull, bool, error) {
	owner, _, _ := strings.Cut(g.Repo, "/")
	q := url.Values{}
	q.Set("state", state)
	q.Set("head", owner+":"+branch)
	q.Set("per_page", strconv.Itoa(pageSize))
	var pulls []Pull
	if err := g.do(ctx, http.MethodGet, "/pulls?"+q.Encode(), nil, &pulls); err != nil {
		return Pull{}, false, err
	}
	if len(pulls) == 0 {
		return Pull{}, false, nil
	}
	return pulls[0], true, nil
}

func (g GitHub) OpenPull(ctx context.Context, title, body, head, base string) (Pull, error) {
	var pull Pull
	in := map[string]string{"title": title, "body": body, "head": head, "base": base}
	return pull, g.do(ctx, http.MethodPost, "/pulls", in, &pull)
}

// Comment uses GitHub's issue comments endpoint, which serves the
// conversation of a pull request under the pull request's number.
func (g GitHub) Comment(ctx context.Context, number int, body string) error {
	return g.do(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/comments", number), map[string]string{"body": body}, nil)
}

// CheckRuns follows pages until a short page comes back.
func (g GitHub) CheckRuns(ctx context.Context, sha string) ([]CheckRun, error) {
	var all []CheckRun
	for page := 1; ; page++ {
		if page > 20 {
			return nil, fmt.Errorf("too many check run pages for %s", sha)
		}
		var resp struct {
			TotalCount int        `json:"total_count"`
			CheckRuns  []CheckRun `json:"check_runs"`
		}
		q := url.Values{}
		q.Set("filter", "latest")
		q.Set("per_page", strconv.Itoa(pageSize))
		q.Set("page", strconv.Itoa(page))
		endpoint := "/commits/" + url.PathEscape(sha) + "/check-runs?" + q.Encode()
		if err := g.do(ctx, http.MethodGet, endpoint, nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.CheckRuns...)
		if len(resp.CheckRuns) < pageSize || (resp.TotalCount > 0 && len(all) >= resp.TotalCount) {
			return all, nil
		}
	}
}

// logReadLimit is how much of a job log JobLog keeps.
const logReadLimit = 8 << 20

// JobLog is answered with a redirect to the log text. It keeps the last
// logReadLimit bytes, since a failure is reported at the end of a log.
func (g GitHub) JobLog(ctx context.Context, id int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	path := fmt.Sprintf("/actions/jobs/%d/logs", id)
	auth, err := g.Bearer(ctx)
	if err != nil {
		return nil, fmt.Errorf("github GET %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(g.API, "/")+"/repos/"+g.Repo+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := readTail(resp.Body, logReadLimit)
	if err != nil {
		return nil, fmt.Errorf("github GET %s: %w", path, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &statusError{code: resp.StatusCode, msg: fmt.Sprintf("github GET %s: %s: %s", path, resp.Status, data)}
	}
	return data, nil
}

// readTail reads r to the end and returns its last limit bytes. The buffer is
// cut back to limit whenever it passes twice that.
func readTail(r io.Reader, limit int) ([]byte, error) {
	var buf []byte
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > 2*limit {
			buf = append(buf[:0], buf[len(buf)-limit:]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if len(buf) > limit {
		buf = buf[len(buf)-limit:]
	}
	return buf, nil
}

// MergePull does not squash or rebase.
func (g GitHub) MergePull(ctx context.Context, number int) error {
	body := map[string]string{"merge_method": "merge"}
	return g.do(ctx, http.MethodPut, fmt.Sprintf("/pulls/%d/merge", number), body, nil)
}

// Protection reads the branch protection endpoint, which needs
// Administration: read. GitHub answers 404 for a branch without protection.
// It answers 403 when the token cannot read protection, which happens when
// the App installation was not granted Administration: read; that is treated
// as no protection too, so merge goes on as it did before this read existed.
func (g GitHub) Protection(ctx context.Context, branch string) (Protection, error) {
	var resp struct {
		Reviews *struct {
			Count int `json:"required_approving_review_count"`
		} `json:"required_pull_request_reviews"`
	}
	endpoint := "/branches/" + escapePath(branch) + "/protection"
	err := g.do(ctx, http.MethodGet, endpoint, nil, &resp)
	var status *statusError
	if errors.As(err, &status) && (status.code == http.StatusNotFound || status.code == http.StatusForbidden) {
		return Protection{}, nil
	}
	if err != nil || resp.Reviews == nil {
		return Protection{}, err
	}
	return Protection{RequiredApprovals: resp.Reviews.Count}, nil
}

// ReadFile resolves branch to its commit, then reads path at that commit
// through the contents API, so the content and the commit match. A 404 on
// the contents is ErrFileNotFound.
func (g GitHub) ReadFile(ctx context.Context, branch, path string) (File, error) {
	var b struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := g.do(ctx, http.MethodGet, "/branches/"+escapePath(branch), nil, &b); err != nil {
		return File{}, err
	}
	var c struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	err := g.do(ctx, http.MethodGet, "/contents/"+escapePath(path)+"?ref="+url.QueryEscape(b.Commit.SHA), nil, &c)
	var status *statusError
	if errors.As(err, &status) && status.code == http.StatusNotFound {
		return File{}, fmt.Errorf("%s at %s (%s): %w", path, branch, b.Commit.SHA, ErrFileNotFound)
	}
	if err != nil {
		return File{}, err
	}
	if c.Encoding != "base64" {
		return File{}, fmt.Errorf("github contents of %s: encoding %q, want base64 (is it larger than 1 MB?)", path, c.Encoding)
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	if err != nil {
		return File{}, fmt.Errorf("github contents of %s: %w", path, err)
	}
	return File{Content: content, Commit: b.Commit.SHA}, nil
}

// escapePath escapes each segment of a slash-separated path for a URL.
func escapePath(p string) string { return strings.ReplaceAll(url.PathEscape(p), "%2F", "/") }

func (g GitHub) RetargetPull(ctx context.Context, number int, base string) error {
	return g.do(ctx, http.MethodPatch, fmt.Sprintf("/pulls/%d", number), map[string]string{"base": base}, nil)
}

// RevokeToken revokes the installation token it is given with DELETE
// /installation/token, so it stops working before it expires. It sends
// token itself, not what Bearer returns. GitHub answers 204 No Content.
func (g GitHub) RevokeToken(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	const path = "/installation/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimSuffix(g.API, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("github DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("github DELETE %s: %s: %s", path, resp.Status, data)
	}
	return nil
}
