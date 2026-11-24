package worker

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
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
// worktree on the planned branch. The clone is per sandbox (one item and
// repository), not shared between items, so schmeide's ref guard is not
// needed.
//
// A session can write anything the worker can (same uid), including the
// clone's config and hooks and directories on PATH (#48). So only git
// commands that talk to the remote get the code host token, through the
// environment, never config or disk, and only after secure: every other
// process in the sandbox is killed (contained mode) and the bare clone is
// reset to state the worker wrote. Every git command runs the binary
// resolved before any session ran, with hooks off and replace refs ignored.

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
	// gitBin is the absolute git binary; sweep, when set, kills every other
	// process in the sandbox before the token leaves the worker.
	gitBin string
	sweep  func(context.Context) error
	// wait retries a remote command that never reached the remote; log
	// says so.
	wait netWait
	log  *slog.Logger
}

// treeSpec says where a sandbox's checkout lives and what it checks out.
type treeSpec struct {
	root, url, base, branch string
	token                   func(context.Context) (codehost.Token, error)
	gitBin                  string
	sweep                   func(context.Context) error
	wait                    netWait
	log                     *slog.Logger
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
		url: s.url, base: s.base, branch: s.branch, token: s.token, gitBin: s.gitBin, sweep: s.sweep,
		wait: s.wait, log: s.log,
	}
	if err := os.MkdirAll(t.root, 0o750); err != nil {
		return nil, fmt.Errorf("create the sandbox directory: %w; check that --work-dir is writable", err)
	}
	if _, err := os.Stat(filepath.Join(t.repoDir, "HEAD")); errors.Is(err, fs.ErrNotExist) {
		if _, err := t.git(ctx, t.root, "init", "-q", "--bare", t.repoDir); err != nil {
			return nil, err
		}
	}
	if err := t.fetchBase(ctx); err != nil {
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
		if _, err := t.remote(ctx, "fetch", "-q", t.url, "+refs/heads/"+t.branch+":"+start); err != nil {
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

// defaultBranch is the branch the remote's HEAD points at. It asks from an
// empty repository of its own, so no clone's config is read.
func defaultBranch(ctx context.Context, s treeSpec) (string, error) {
	scratch, err := os.MkdirTemp("", "factory-ls-remote-")
	if err != nil {
		return "", fmt.Errorf("create a scratch repository: %w", err)
	}
	defer os.RemoveAll(scratch)
	t := &tree{
		root: scratch, repoDir: scratch, url: s.url, token: s.token, gitBin: s.gitBin, sweep: s.sweep,
		wait: s.wait, log: s.log,
	}
	if _, err := t.git(ctx, scratch, "init", "-q", "--bare", scratch); err != nil {
		return "", err
	}
	out, err := t.remote(ctx, "ls-remote", "--symref", s.url, "HEAD")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			branch, _, _ := strings.Cut(ref, "\t")
			return branch, nil
		}
	}
	return "", fmt.Errorf("%s has no default branch; push one", s.url)
}

func (t *tree) baseRef() string { return "refs/remotes/origin/" + t.base }

// basicAuth is the HTTP Basic credential git sends for an installation token.
func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// git runs one local git command in dir, as the commit identity the server
// sent with the token, or factory without one. It never carries the token.
func (t *tree) git(ctx context.Context, dir string, args ...string) (string, error) {
	token, err := t.token(ctx)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	name, email := gitName, gitEmail
	if id := token.CommitIdentity; id != nil && id.Name != "" && id.Email != "" {
		name, email = id.Name, id.Email
	}
	env := append(filterEnv(os.Environ(), baseEnv(), basePrefixes()),
		"GIT_AUTHOR_NAME="+name, "GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME="+name, "GIT_COMMITTER_EMAIL="+email,
	)
	return t.run(ctx, dir, env, args)
}

// remote runs one git command that talks to the remote, with the step's
// current token, in the bare clone, after secure. The token goes in through
// the environment, so it is never written to git config or shown in the
// process list, and it is read for each command, so a push late in a long
// step does not send an expired one. PATH is only git's own directory and
// the system's, so nothing a session put on PATH runs.
func (t *tree) remote(ctx context.Context, args ...string) (string, error) {
	token, err := t.token(ctx)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if err := t.secure(ctx); err != nil {
		return "", err
	}
	env := []string{
		"PATH=" + filepath.Dir(t.gitPath()) + ":/usr/bin:/bin",
		"GIT_DIR=" + t.repoDir,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basicAuth(token.Token),
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	// A fresh pod can have no DNS for its first seconds (#76): a command
	// that never reached the remote is run again for a while.
	var out string
	err = t.wait.do(ctx, t.log, "git "+args[0], gitUnreachable, func() error {
		var err error
		out, err = t.run(ctx, t.repoDir, env, args)
		return err
	})
	return out, err
}

// run runs the git binary in dir with env and the settings every git
// command of the worker shares: no global or system config, no prompts, no
// hooks, no replace refs.
func (t *tree) run(ctx context.Context, dir string, env, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, t.gitPath(), append([]string{"-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Dir = dir
	cmd.Env = slices.Concat(env, []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_NO_REPLACE_OBJECTS=1",
	})
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (t *tree) gitPath() string {
	if t.gitBin == "" {
		return "git"
	}
	return t.gitBin
}

// cleanConfig is the bare clone's whole config: nothing in it runs a
// command, rewrites a URL, or adds a credential.
const cleanConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n"

// secure makes the sandbox safe for the code host token: every other
// process is killed (when the worker is contained), so nothing a session or
// gate left running can read a git child's environment, and the bare clone
// loses everything a session could have planted in it that git would run or
// that would make git see other objects than it pushes: its config is
// rewritten and its hooks, attributes, grafts, shallow file, alternates and
// replace refs are removed. Objects and refs stay: they are data.
func (t *tree) secure(ctx context.Context) error {
	if t.sweep != nil {
		if err := t.sweep(ctx); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(t.repoDir, "config"), []byte(cleanConfig), 0o600); err != nil {
		return fmt.Errorf("reset the clone's config: %w", err)
	}
	for _, p := range []string{
		"hooks", "info/attributes", "info/grafts", "shallow", "commondir", "config.worktree",
		"objects/info/alternates", "objects/info/http-alternates", "refs/replace",
	} {
		if err := os.RemoveAll(filepath.Join(t.repoDir, filepath.FromSlash(p))); err != nil {
			return fmt.Errorf("reset the clone: %w", err)
		}
	}
	return nil
}

// fetchBase fetches the base branch from the remote into the clone.
func (t *tree) fetchBase(ctx context.Context) error {
	_, err := t.remote(ctx, "fetch", "-q", t.url, "+refs/heads/"+t.base+":"+t.baseRef())
	return err
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

// changed lists the paths an uncommitted change modifies or deletes,
// including the old side of a rename; a new file is not listed. It stages
// the change to see them.
func (t *tree) changed(ctx context.Context) ([]string, error) {
	if _, err := t.git(ctx, t.dir, "add", "-A"); err != nil {
		return nil, err
	}
	out, err := t.git(ctx, t.dir, "diff", "--cached", "--no-renames", "--diff-filter=a", "--name-only")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
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

// push publishes the branch's HEAD. The commits it adds are scanned first,
// in the secured clone; a finding refuses the push. A remote branch that is
// not an ancestor of HEAD was not written by this checkout and is left in
// place.
func (t *tree) push(ctx context.Context) error {
	head, err := t.scanSecrets(ctx)
	if err != nil {
		return err
	}
	remote, err := t.remoteHead(ctx)
	if err != nil {
		return err
	}
	if remote != "" && !t.ancestorOf(ctx, remote, head) {
		return fmt.Errorf("%w: %s has commits this checkout does not, so it was not overwritten; someone has to look at that branch", ErrBranchMoved, t.branch)
	}
	_, err = t.remote(ctx, "push", "-q", t.url, head+":refs/heads/"+t.branch)
	return err
}

// scanSecrets scans the commits HEAD adds to the base for credential-shaped
// literals and for the step's own token, and returns HEAD. It first secures
// the sandbox and fetches the base again, so what it reads is the remote's
// base and the clone as the worker left it, not as a session did.
func (t *tree) scanSecrets(ctx context.Context) (string, error) {
	head, err := t.head(ctx)
	if err != nil {
		return "", err
	}
	if err := t.fetchBase(ctx); err != nil {
		return "", err
	}
	token, err := t.token(ctx)
	if err != nil {
		return "", err
	}
	found, err := secretscan.ScanDiff(ctx, t.gitPath(), t.repoDir, t.baseRef(), head, []string{token.Token})
	if err != nil {
		return "", fmt.Errorf("scan the branch's changes for secrets: %w", err)
	}
	if len(found) == 0 {
		return head, nil
	}
	parts := make([]string, len(found))
	for i, f := range found {
		parts[i] = fmt.Sprintf("%s:%d contains a %s", f.File, f.Line, f.Kind)
	}
	return "", fmt.Errorf("%w: %s", ErrSecretFound, strings.Join(parts, "; "))
}

// remoteHead is the commit the remote branch points at, or "" when there is
// no such branch.
func (t *tree) remoteHead(ctx context.Context) (string, error) {
	ref := "refs/heads/" + t.branch
	out, err := t.remote(ctx, "ls-remote", t.url, ref)
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

// ancestorOf reports whether commit is head or an ancestor of it.
func (t *tree) ancestorOf(ctx context.Context, commit, head string) bool {
	if _, err := t.git(ctx, t.repoDir, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return false
	}
	_, err := t.git(ctx, t.repoDir, "merge-base", "--is-ancestor", commit, head)
	return err == nil
}

// errGateTimeout is a gate canceled at its deadline.
var errGateTimeout = errors.New("the gate timed out")

// runGate runs the gate command in the worktree, in its own process group
// with a scrubbed environment and a scratch HOME, and returns its combined
// output, cut to its head and tail past maxGateOutput. At limit the group is
// killed.
func (t *tree) runGate(ctx context.Context, command string, limit time.Duration) (*capture, error) {
	out := newCapture(maxGateOutput)
	scratch := filepath.Join(t.root, "scratch")
	if err := os.RemoveAll(scratch); err != nil {
		return out, fmt.Errorf("clear the gate's scratch directory: %w", err)
	}
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return out, fmt.Errorf("create the gate's scratch directory: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = t.dir
	cmd.Env = gateEnv(os.Environ(), scratch)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("%w after %s", errGateTimeout, limit)
	}
	return out, err
}
