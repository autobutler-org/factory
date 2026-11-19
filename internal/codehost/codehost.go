// Package codehost talks to the code host, where a work item's code goes: pull
// requests and their comments, check runs and their logs, merge, the merge
// queue, branch protection, one file at a branch (for the server to read a
// repository's config), and GitHub App installation tokens. GitHub
// implements Host over HTTP for one repository, asking for the token before
// each call; TokenMinter mints those tokens and RefreshingToken keeps one
// fresh. Fake implements Host in memory for tests, and the fakegh
// subpackage is a fake GitHub API server.
//
// It knows nothing about trackers. Work can come from any tracker or from a
// plain API call, so nothing here takes a tracker's ticket number: branches
// are named from the work item (plan.BranchName), a pull request is found by its
// branch, and the base of stacked work is chosen by the caller. PullBody
// links the item's source by URL. It does not decide who may get a token (internal/work does)
// and records no outputs (the worker reports them to the API).
package codehost

import (
	"context"
	"errors"
)

// ErrFileNotFound is returned by ReadFile for a path the branch does not
// have. Any other ReadFile error means the code host could not be read.
var ErrFileNotFound = errors.New("file not found")

// Host is one repository on a code host, seen with one worker's token.
type Host interface {
	// DefaultBranch is the repository's default branch.
	DefaultBranch(ctx context.Context) (string, error)
	// FindPull returns the newest pull request in state ("open", "closed",
	// or "all") whose head is branch in this repository. ok is false when
	// there is none.
	FindPull(ctx context.Context, branch, state string) (pull Pull, ok bool, err error)
	// OpenPull opens a pull request from head into base.
	OpenPull(ctx context.Context, title, body, head, base string) (Pull, error)
	// Comment posts a comment on pull request number.
	Comment(ctx context.Context, number int, body string) error
	// CheckRuns lists the latest check run for each name on sha.
	CheckRuns(ctx context.Context, sha string) ([]CheckRun, error)
	// JobLog reads the end of the log of the Actions job behind check run id.
	JobLog(ctx context.Context, id int64) ([]byte, error)
	// QueuedPull is the node ID of pull request number when its base branch
	// has a merge queue, or "" when it does not.
	QueuedPull(ctx context.Context, number int) (string, error)
	// EnqueuePull adds the pull request with node ID id to its merge queue.
	EnqueuePull(ctx context.Context, id string) error
	// MergePull merges pull request number with a merge commit.
	MergePull(ctx context.Context, number int) error
	// Protection reads branch protection on branch. An unprotected branch
	// is the zero Protection and no error.
	Protection(ctx context.Context, branch string) (Protection, error)
	// RetargetPull changes the base branch of pull request number, as when
	// the pull request it was stacked on merges.
	RetargetPull(ctx context.Context, number int, base string) error
	// ReadFile reads the file at path in the commit branch points at, or
	// returns ErrFileNotFound.
	ReadFile(ctx context.Context, branch, path string) (File, error)
}

// File is one file read from the code host.
type File struct {
	Content []byte
	// Commit is the SHA of the commit the file was read at.
	Commit string
}

// Pull is a pull request.
type Pull struct {
	Number    int    `json:"number"`
	HTMLURL   string `json:"html_url"`
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
	// MergedAt is when the pull request merged, empty when it has not. The
	// pull request list carries it; it does not carry "merged".
	MergedAt string `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// CheckRun is one check run on a commit. Its ID is also the Actions job ID
// that JobLog reads.
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// Protection is the part of branch protection a worker reads.
type Protection struct {
	// RequiredApprovals is how many approving reviews a pull request needs
	// before it can merge. Zero requires none.
	RequiredApprovals int
}
