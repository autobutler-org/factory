//go:build linux

package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sweepTimeout bounds how long sweep waits for killed processes to go.
const sweepTimeout = 5 * time.Second

// protect makes the worker non-dumpable: a process of the same uid without
// CAP_SYS_PTRACE can no longer read its /proc/<pid>/environ, mem or maps, or
// ptrace it. The flag is this process's only; children get their own at
// exec, so sessions are unaffected.
func protect() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); errno != 0 {
		return fmt.Errorf("make the worker non-dumpable: %w", errno)
	}
	return nil
}

// probe exits 3 when it can read the environment of process $1, 4 when it
// can open its memory, and 0 when it can do neither.
const probe = `cat "/proc/$1/environ" >/dev/null 2>&1 && exit 3; (: <"/proc/$1/mem") 2>/dev/null && exit 4; exit 0`

// checkProtected starts a child the way a session is started and has it
// read this process's environment and memory. It is an error when either
// read works: this kernel or runtime does not honor non-dumpable, or the
// child has CAP_SYS_PTRACE.
func checkProtected(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", probe, "sh", strconv.Itoa(os.Getpid()))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exit) && exit.ExitCode() == 3:
		return fmt.Errorf("a child process can read the worker's environment (/proc/%d/environ)", os.Getpid())
	case errors.As(err, &exit) && exit.ExitCode() == 4:
		return fmt.Errorf("a child process can open the worker's memory (/proc/%d/mem)", os.Getpid())
	}
	return fmt.Errorf("check that a child cannot read the worker: %w", err)
}

// checkGit is an error when a session could replace the git binary the
// worker runs: the file or its directory is writable by this user.
func checkGit(gitBin string) error {
	for _, p := range []string{gitBin, filepath.Dir(gitBin)} {
		if syscall.Access(p, 2) == nil { // W_OK
			return fmt.Errorf("%s is writable by the worker's user, so a session could replace git; install git where only root can write", p)
		}
	}
	return nil
}

// sweep kills every other process of this user in the container but PID 1
// (tini), and waits until none is left: whatever a session or gate started,
// daemonized or not. kill(-1) reaches every process the caller may signal
// except itself and PID 1, so it is only safe in a container of the
// worker's own (--contained).
func sweep(ctx context.Context) error {
	deadline := time.Now().Add(sweepTimeout)
	for {
		_ = syscall.Kill(-1, syscall.SIGKILL)
		left, err := others()
		if err != nil {
			return err
		}
		if len(left) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes %v outlived SIGKILL, so the code host token was not handed to git", left)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// others lists the live processes of this user other than this one and
// PID 1. Zombies are dead and not listed.
func others() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	uid, self := strconv.Itoa(os.Getuid()), os.Getpid()
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self || pid == 1 {
			continue
		}
		status, err := os.ReadFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue // gone already
		}
		var state, owner string
		for line := range strings.SplitSeq(string(status), "\n") {
			if v, ok := strings.CutPrefix(line, "State:"); ok {
				state = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "Uid:"); ok {
				if f := strings.Fields(v); len(f) > 0 {
					owner = f[0]
				}
			}
		}
		if owner == uid && !strings.HasPrefix(state, "Z") && !strings.HasPrefix(state, "X") {
			out = append(out, pid)
		}
	}
	return out, nil
}
