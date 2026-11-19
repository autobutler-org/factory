package codehost

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

// Fake is an in-memory Host for tests. It makes no network calls. Set its
// exported fields before handing it to the code under test, and read what
// that code did with Comments, Merged, and Enqueued. The zero Fake is a
// repository on main with no pull requests, no check runs, no merge queue,
// and no branch protection.
type Fake struct {
	// Repo is "owner/name", used in the URLs of opened pull requests. Empty
	// is "o/r".
	Repo string
	// Default is the default branch. Empty is "main".
	Default string
	// Pulls are numbered by position: Pulls[0] is pull request 1.
	Pulls []Pull
	// Checks are the check runs every commit has.
	Checks []CheckRun
	// Logs are the Actions job logs by job ID. A job without one is an error.
	Logs map[int64]string
	// QueueID, when set, is the node ID QueuedPull returns: the base branch
	// has a merge queue.
	QueueID string
	// Protections is the branch protection by branch.
	Protections map[string]Protection
	// Files are the default branch's files by path, for ReadFile. ReadErr,
	// when set, is the error every ReadFile returns, as in an outage.
	Files   map[string]string
	ReadErr error

	mu       sync.Mutex
	comments map[int][]string
	merged   []int
	enqueued []string
}

var _ Host = (*Fake)(nil)

func (f *Fake) DefaultBranch(context.Context) (string, error) {
	if f.Default == "" {
		return "main", nil
	}
	return f.Default, nil
}

// FindPull returns the last matching pull request, as GitHub lists the
// newest first.
func (f *Fake) FindPull(_ context.Context, branch, state string) (Pull, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range slices.Backward(f.Pulls) {
		if p.Head.Ref == branch && (state == "all" || state == p.State) {
			return p, true, nil
		}
	}
	return Pull{}, false, nil
}

func (f *Fake) OpenPull(_ context.Context, _, _, head, base string) (Pull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	repo := f.Repo
	if repo == "" {
		repo = "o/r"
	}
	n := len(f.Pulls) + 1
	p := Pull{Number: n, HTMLURL: fmt.Sprintf("https://github.test/%s/pull/%d", repo, n), State: "open"}
	p.Head.Ref, p.Base.Ref = head, base
	f.Pulls = append(f.Pulls, p)
	return p, nil
}

func (f *Fake) Comment(_ context.Context, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.comments == nil {
		f.comments = map[int][]string{}
	}
	f.comments[number] = append(f.comments[number], body)
	return nil
}

func (f *Fake) CheckRuns(context.Context, string) ([]CheckRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Checks), nil
}

func (f *Fake) JobLog(_ context.Context, id int64) ([]byte, error) {
	log, ok := f.Logs[id]
	if !ok {
		return nil, fmt.Errorf("no log for job %d", id)
	}
	return []byte(log), nil
}

func (f *Fake) QueuedPull(context.Context, int) (string, error) { return f.QueueID, nil }

func (f *Fake) EnqueuePull(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enqueued = append(f.enqueued, id)
	return nil
}

func (f *Fake) MergePull(_ context.Context, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged = append(f.merged, number)
	if number >= 1 && number <= len(f.Pulls) {
		f.Pulls[number-1].State = "closed"
		f.Pulls[number-1].MergedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return nil
}

func (f *Fake) Protection(_ context.Context, branch string) (Protection, error) {
	return f.Protections[branch], nil
}

func (f *Fake) RetargetPull(_ context.Context, number int, base string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if number < 1 || number > len(f.Pulls) {
		return fmt.Errorf("no pull request %d", number)
	}
	f.Pulls[number-1].Base.Ref = base
	return nil
}

// Comments returns the comments posted so far by pull request number.
func (f *Fake) Comments() map[int][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := maps.Clone(f.comments)
	for k, v := range out {
		out[k] = slices.Clone(v)
	}
	return out
}

// Merged returns the pull request numbers merged so far.
func (f *Fake) Merged() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.merged)
}

// Enqueued returns the node IDs added to the merge queue so far.
func (f *Fake) Enqueued() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.enqueued)
}

// FakeCommit is the commit Fake.ReadFile reports.
const FakeCommit = "fa4efa4efa4efa4efa4efa4efa4efa4efa4efa4e"

// ReadFile reads Files, whatever the branch.
func (f *Fake) ReadFile(_ context.Context, _, path string) (File, error) {
	if f.ReadErr != nil {
		return File{}, f.ReadErr
	}
	content, ok := f.Files[path]
	if !ok {
		return File{}, fmt.Errorf("%s: %w", path, ErrFileNotFound)
	}
	return File{Content: []byte(content), Commit: FakeCommit}, nil
}
