package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/repoconfig"
	"github.com/autobutler-org/factory/internal/secretscan"
)

// Ported from schmeide's internal/runner/workspace.go: a bare clone and one
// worktree on the planned branch, with the code host token handed to git
// per command through the environment, never written to git config, disk or
// the process list. The clone is per sandbox (one item and repository), not
// shared between items, so schmeide's ref guard is not needed.

const (
	// gitTimeout bounds one git command.
	gitTimeout = 10 * time.Minute
	// gitName and gitEmail author the worker's commits when the server
	// sends no commit identity with the token.
	gitName  = "factory"
	gitEmail = "factory@users.noreply.github.com"
)

var (
	// ErrSecretFound is a push refused because the change adds a
	// credential-shaped literal or the worker's own token. Retrying does
	// not change it.
	ErrSecretFound = errors.New("the change contains a secret")
	// ErrBranchMoved is a push refused because the remote branch has commits
	// this checkout does not. Someone has to look at the branch.
	ErrBranchMoved = errors.New("the remote branch moved")
	// ErrNoConfig is a repository without .factory/config.toml on its base
	// branch.
	ErrNoConfig = errors.New("the repository has no " + repoconfig.Path)
)

// tree is one sandbox's checkout: a bare clone at root/repo.git and a
// worktree at root/tree on branch.
type tree struct {
	root, repoDir, dir string
	url, base, branch  string
	// token returns the step's current code host token and the identity
	// to commit as.
	token func(context.Context) (codehost.Token, error)
}

// treeSpec says where a sandbox's checkout lives and what it checks out.
type treeSpec struct {
	root, url, base, branch string
	token                   func(context.Context) (codehost.Token, error)
}

// openTree returns the sandbox's checkout on spec.branch, creating what is
// missing: the bare clone, the base branch, and the worktree. An existing
// worktree on the branch is reused as it is, so a step continues on what an
// earlier step in the same sandbox left. A missing one starts from the
// remote branch when it exists, which is how a step in a fresh sandbox
// resumes, and otherwise from the base.
func openTree(ctx context.Context, s treeSpec) (*tree, error) {
	t := &tree{
		root: s.root, repoDir: filepath.Join(s.root, "repo.git"), dir: filepath.Join(s.root, "tree"),
		url: s.url, base: s.base, branch: s.branch, token: s.token,
	}
	if err := os.MkdirAll(t.root, 0o750); err != nil {
		return nil, fmt.Errorf("create the sandbox directory: %w; check that --work-dir is writable", err)
	}
	if _, err := os.Stat(filepath.Join(t.repoDir, "HEAD")); errors.Is(err, fs.ErrNotExist) {
		if _, err := t.git(ctx, t.root, "init", "-q", "--bare", t.repoDir); err != nil {
			return nil, err
		}
	}
	if _, err := t.git(ctx, t.repoDir, "fetch", "-q", t.url, "+refs/heads/"+t.base+":"+t.baseRef()); err != nil {
		return nil, err
	}
	if current, err := t.git(ctx, t.dir, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && current == t.branch {
		return t, nil
	}
	start := t.baseRef()
	remote, err := t.remoteHead(ctx)
	if err != nil {
		return nil, err
	}
	if remote != "" {
		start = "refs/remotes/origin/" + t.branch
		if _, err := t.git(ctx, t.repoDir, "fetch", "-q", t.url, "+refs/heads/"+t.branch+":"+start); err != nil {
			return nil, err
		}
	}
	if err := os.RemoveAll(t.dir); err != nil {
		return nil, fmt.Errorf("clear a leftover worktree: %w", err)
	}
	for _, args := range [][]string{
		{"worktree", "prune"},
		{"worktree", "add", "-q", "-B", t.branch, t.dir, start},
	} {
		if _, err := t.git(ctx, t.repoDir, args...); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// defaultBranch is the branch the remote's HEAD points at.
func defaultBranch(ctx context.Context, root, url string, token func(context.Context) (codehost.Token, error)) (string, error) {
	t := &tree{root: root, url: url, token: token}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return "", fmt.Errorf("create the sandbox directory: %w; check that --work-dir is writable", err)
	}
	out, err := t.git(ctx, root, "ls-remote", "--symref", url, "HEAD")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			branch, _, _ := strings.Cut(ref, "\t")
			return branch, nil
		}
	}
	return "", fmt.Errorf("%s has no default branch; push one", url)
}

func (t *tree) baseRef() string { return "refs/remotes/origin/" + t.base }

// basicAuth is the HTTP Basic credential git sends for an installation token.
func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// git runs one git command in dir with the step's current token, as the
// commit identity the server sent with it, or factory without one. The token
// goes in through the environment, so it is never written to git config or
// shown in the process list, and it is read for each command, so a push late
// in a long step does not send an expired one.
func (t *tree) git(ctx context.Context, dir string, args ...string) (string, error) {
	token, err := t.token(ctx)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	name, email := gitName, gitEmail
	if id := token.CommitIdentity; id != nil && id.Name != "" && id.Email != "" {
		name, email = id.Name, id.Email
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(filterEnv(os.Environ(), baseEnv(), basePrefixes()),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME="+name, "GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+email,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+basicAuth(token.Token),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// config reads .factory/config.toml from the base branch, not the worktree,
// so a session cannot change the gate it is held to.
func (t *tree) config(ctx context.Context) (repoconfig.Config, error) {
	object := t.baseRef() + ":" + repoconfig.Path
	if _, err := t.git(ctx, t.repoDir, "cat-file", "-e", object); err != nil {
		return repoconfig.Config{}, fmt.Errorf("%w on %s; add one with a gate command, e.g. gate = \"make check\"", ErrNoConfig, t.base)
	}
	data, err := t.git(ctx, t.repoDir, "show", object)
	if err != nil {
		return repoconfig.Config{}, err
	}
	return repoconfig.Parse(repoconfig.Path+" on "+t.base, []byte(data))
}

// commit commits whatever is uncommitted with message, signed off by the
// committing identity, and reports whether the branch now has commits its
// base does not.
func (t *tree) commit(ctx context.Context, message string) (bool, error) {
	if _, err := t.git(ctx, t.dir, "add", "-A"); err != nil {
		return false, err
	}
	dirty, err := t.git(ctx, t.dir, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if dirty != "" {
		if _, err := t.git(ctx, t.dir, "-c", "commit.gpgsign=false", "commit", "-q", "-s", "-m", message); err != nil {
			return false, err
		}
	}
	n, err := t.ahead(ctx)
	return n > 0, err
}

// ahead is how many commits the branch has that its base does not.
func (t *tree) ahead(ctx context.Context) (int, error) {
	out, err := t.git(ctx, t.dir, "rev-list", "--count", t.baseRef()+"..HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}

// clean reports whether the worktree has no uncommitted change.
func (t *tree) clean(ctx context.Context) (bool, error) {
	out, err := t.git(ctx, t.dir, "status", "--porcelain")
	return out == "", err
}

// discard drops every uncommitted change, as after a review that must leave
// the branch as it found it.
func (t *tree) discard(ctx context.Context) error {
	if _, err := t.git(ctx, t.dir, "reset", "-q", "--hard"); err != nil {
		return err
	}
	_, err := t.git(ctx, t.dir, "clean", "-fdq")
	return err
}

// hasSubject reports whether a commit after the base has subject as its
// subject: whether an earlier attempt already made that commit.
func (t *tree) hasSubject(ctx context.Context, subject string) (bool, error) {
	out, err := t.git(ctx, t.dir, "log", "--format=%s", t.baseRef()+"..HEAD")
	if err != nil {
		return false, err
	}
	return slices.Contains(strings.Split(out, "\n"), subject), nil
}

// firstMessage is the subject and body of the branch's first commit after
// the base without its trailing sign-off, whoever signed it: the pull
// request text the implement step committed.
func (t *tree) firstMessage(ctx context.Context) (subject, body string, err error) {
	out, err := t.git(ctx, t.dir, "rev-list", "--reverse", t.baseRef()+"..HEAD")
	if err != nil || out == "" {
		return "", "", err
	}
	first, _, _ := strings.Cut(out, "\n")
	msg, err := t.git(ctx, t.dir, "log", "-1", "--format=%B", first)
	subject, body, _ = strings.Cut(msg, "\n")
	body = strings.TrimSpace(body)
	if i := strings.LastIndex(body, "\n") + 1; strings.HasPrefix(body[i:], "Signed-off-by: ") {
		body = body[:i]
	}
	return strings.TrimSpace(subject), strings.TrimSpace(body), err
}

// head is the commit HEAD points at.
func (t *tree) head(ctx context.Context) (string, error) {
	return t.git(ctx, t.dir, "rev-parse", "HEAD")
}

// push publishes the branch. The lines it adds are scanned first; a finding
// refuses the push. A remote branch that is not an ancestor of HEAD was not
// written by this checkout and is left in place.
func (t *tree) push(ctx context.Context) error {
	if err := t.scanSecrets(ctx); err != nil {
		return err
	}
	remote, err := t.remoteHead(ctx)
	if err != nil {
		return err
	}
	if remote != "" && !t.ancestorOfHEAD(ctx, remote) {
		return fmt.Errorf("%w: %s has commits this checkout does not, so it was not overwritten; someone has to look at that branch", ErrBranchMoved, t.branch)
	}
	_, err = t.git(ctx, t.dir, "push", "-q", t.url, "HEAD:refs/heads/"+t.branch)
	return err
}

// scanSecrets scans the lines the branch adds relative to its base for
// credential-shaped literals and for the step's own token.
func (t *tree) scanSecrets(ctx context.Context) error {
	token, err := t.token(ctx)
	if err != nil {
		return err
	}
	found, err := secretscan.ScanDiff(ctx, t.dir, t.baseRef(), []string{token.Token})
	if err != nil {
		return fmt.Errorf("scan the branch's changes for secrets: %w", err)
	}
	if len(found) == 0 {
		return nil
	}
	parts := make([]string, len(found))
	for i, f := range found {
		parts[i] = fmt.Sprintf("%s:%d contains a %s", f.File, f.Line, f.Kind)
	}
	return fmt.Errorf("%w: %s", ErrSecretFound, strings.Join(parts, "; "))
}

// remoteHead is the commit the remote branch points at, or "" when there is
// no such branch.
func (t *tree) remoteHead(ctx context.Context) (string, error) {
	ref := "refs/heads/" + t.branch
	out, err := t.git(ctx, t.repoDir, "ls-remote", t.url, ref)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		sha, got, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if ok && got == ref && sha != "" {
			return sha, nil
		}
	}
	return "", nil
}

// ancestorOfHEAD reports whether commit is HEAD or an ancestor of it.
func (t *tree) ancestorOfHEAD(ctx context.Context, commit string) bool {
	if _, err := t.git(ctx, t.dir, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return false
	}
	_, err := t.git(ctx, t.dir, "merge-base", "--is-ancestor", commit, "HEAD")
	return err == nil
}

// gateTailBytes is how much of the gate's output a failure reports.
const gateTailBytes = 4000

// errGateTimeout is a gate canceled at its deadline.
var errGateTimeout = errors.New("the gate timed out")

// runGate runs the gate command in the worktree, in its own process group
// with a scrubbed environment and a scratch HOME, and returns its combined
// output. At limit the group is killed.
func (t *tree) runGate(ctx context.Context, command string, limit time.Duration) (string, error) {
	scratch := filepath.Join(t.root, "scratch")
	if err := os.RemoveAll(scratch); err != nil {
		return "", fmt.Errorf("clear the gate's scratch directory: %w", err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return "", fmt.Errorf("create the gate's scratch directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = t.dir
	cmd.Env = gateEnv(os.Environ(), scratch)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return string(out), fmt.Errorf("%w after %s", errGateTimeout, limit)
	}
	return string(out), err
}

// tail is the end of s, at most n bytes.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...\n" + s[len(s)-n:]
}
