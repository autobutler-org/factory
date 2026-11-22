//go:build linux

package worker

import (
	"bufio"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const helperEnv = "FACTORY_TEST_WORKER_HELPER"

// TestHelperProcess is the stand-in worker the tests below start: it holds a
// secret in its environment, makes itself non-dumpable when asked, reports
// its own startup check, and waits.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		t.Skip("run by the containment tests")
	}
	if mode == "protect" {
		if err := protect(); err != nil {
			t.Fatal(err)
		}
	}
	check := "ok"
	if err := checkProtected(t.Context()); err != nil {
		check = err.Error()
	}
	_, _ = os.Stdout.WriteString(check + "\n")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

// startHelper starts the stand-in worker and returns its pid and its check.
func startHelper(t *testing.T, mode string) (int, string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, "FACTORY_SECRET_FOR_TEST=s3cr3t-worker-credential")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	return cmd.Process.Pid, strings.TrimSpace(line)
}

// TestProtectStopsASameUIDRead reproduces #48 and shows it closed: a process
// of the worker's user reads a dumpable worker's environment and memory, and
// can read neither once the worker is non-dumpable. The worker's own check
// tells the two apart.
func TestProtectStopsASameUIDRead(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root has CAP_SYS_PTRACE and reads any process; run as a user, as the worker does")
	}
	pid, check := startHelper(t, "plain")
	proc := "/proc/" + strconv.Itoa(pid)
	env, err := os.ReadFile(proc + "/environ")
	if err != nil || !strings.Contains(string(env), "s3cr3t-worker-credential") {
		t.Fatalf("reading a dumpable worker's environment: %v; the hole this test pins should be open", err)
	}
	if mem, err := os.Open(proc + "/mem"); err != nil {
		t.Errorf("opening a dumpable worker's memory: %v", err)
	} else {
		_ = mem.Close()
	}
	if !strings.Contains(check, "environ") {
		t.Errorf("a dumpable worker's check = %q, want it to report the read", check)
	}

	pid, check = startHelper(t, "protect")
	proc = "/proc/" + strconv.Itoa(pid)
	if _, err := os.ReadFile(proc + "/environ"); !os.IsPermission(err) {
		t.Errorf("reading a protected worker's environment = %v, want permission denied", err)
	}
	if _, err := os.Open(proc + "/mem"); !os.IsPermission(err) {
		t.Errorf("opening a protected worker's memory = %v, want permission denied", err)
	}
	if check != "ok" {
		t.Errorf("a protected worker's check = %q, want ok", check)
	}
}

func TestOthersListsLiveProcesses(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	left, err := others()
	if err != nil || !slices.Contains(left, pid) || slices.Contains(left, os.Getpid()) {
		t.Errorf("others() = %v, %v; want the child %d and not this process", left, err, pid)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if left, _ := others(); slices.Contains(left, pid) {
		t.Errorf("others() still lists %d after it was reaped", pid)
	}
}

// TestSweepKillsEveryOtherProcess kills everything of this user but itself
// and PID 1, so it runs only in a container of its own: make test/contained.
func TestSweepKillsEveryOtherProcess(t *testing.T) {
	if os.Getenv("FACTORY_TEST_CONTAINED") != "1" {
		t.Skip("kills every process of this user; run it with make test/contained")
	}
	// What a session leaves: a background child and a daemon in its own
	// session, which escapes a process-group kill.
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 600 >/dev/null 2>&1 & setsid sleep 600 >/dev/null 2>&1 & echo started")
	if out, err := cmd.Output(); err != nil || !strings.Contains(string(out), "started") {
		t.Fatalf("start the leftovers: %v %s", err, out)
	}
	time.Sleep(200 * time.Millisecond)
	if left, _ := others(); len(left) < 2 {
		t.Fatalf("others() = %v before the sweep, want the two sleeps", left)
	}
	if err := sweep(t.Context()); err != nil {
		t.Fatal(err)
	}
	if left, _ := others(); len(left) != 0 {
		t.Errorf("others() = %v after the sweep, want none", left)
	}
}
