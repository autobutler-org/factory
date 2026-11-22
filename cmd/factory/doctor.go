package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/autobutler-org/factory/internal/apiclient"
	"github.com/autobutler-org/factory/internal/doctor"
)

// notChecked are the doctor checks #16 names whose feature has not landed.
// Each lands with its feature; until then doctor says it did not check,
// rather than pass.
func notChecked() []string {
	return []string{
		"each configured adapter has its secret (adapters: #7, #8)",
		"the stored sign-in is valid and its role (sign-in: #15)",
		"the telemetry exporter is delivering (telemetry: #13)",
	}
}

func doctorCommand() command {
	return command{
		name:    "doctor",
		summary: "Check the setup and say how to fix what is wrong.",
		about: `Check that the server is reachable, its version is compatible, the token is
accepted, and the server's own checks: its database is reachable and migrated,
the code host credential works, the sandbox runtime answers, and a cluster
worker is active. With --worker, check this machine as a worker image instead:
git, the agent CLIs and their logins, and the workspace directory.

Each check prints pass, fail or skip, and a failure says how to fix it. Exits 1
when any check fails, after naming every failing check. On a terminal, a failure
with a factory command that fixes it offers to run it.`,
		env: clientEnv,
		setup: func(fs *flag.FlagSet) runner {
			cf := addClientFlags(fs)
			asWorker := fs.Bool("worker", false, "check this machine as a worker image instead of checking the server")
			workDir := fs.String("work-dir", "", "with --worker, the worker's checkout `directory` (default $FACTORY_WORK_DIR, else $TMPDIR/factory-worker)")
			harnesses := fs.String("harnesses", "", "with --worker, the agent CLIs to check (default $FACTORY_HARNESSES, else claude-code,grok)")
			return func(ctx context.Context, _ []string, s streams) int {
				if *asWorker {
					return workerDoctor(ctx, s, *cf.output, *workDir, *harnesses)
				}
				c, code := cf.connect(s, "doctor")
				if c == nil {
					return code
				}
				return report(ctx, s, c.json, serverDoctor(ctx, c), notChecked())
			}
		},
	}
}

// serverDoctor runs the client's checks and fetches the server's.
func serverDoctor(ctx context.Context, c *client) []doctor.Result {
	reach := doctor.Result{Name: "server reachable", Status: doctor.Pass, Detail: c.URL}
	if _, err := c.Call(ctx, http.MethodGet, "/healthz", nil, nil, nil); err != nil {
		reach.Status, reach.Detail = doctor.Fail, c.explain(err, "Check that --server or FACTORY_SERVER is the factory server's URL.")
		reach.Fix = "start the server, or point --server or FACTORY_SERVER at it"
		skipped := "not checked: the server is unreachable"
		return []doctor.Result{
			reach,
			{Name: "token accepted", Status: doctor.Skip, Detail: skipped},
			{Name: "server checks", Status: doctor.Skip, Detail: skipped},
		}
	}
	var answer struct {
		Version string          `json:"version"`
		Checks  []doctor.Result `json:"checks"`
	}
	_, err := c.Call(ctx, http.MethodGet, "/api/v1/admin/doctor", nil, nil, &answer)
	token := doctor.Result{Name: "token accepted", Status: doctor.Pass}
	if os.Getenv(apiTokenEnv) == "" {
		token.Detail = "the server asks for no token: it is unauthenticated, on loopback"
	}
	var apiErr *apiclient.Error
	switch {
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized:
		token.Status, token.Detail = doctor.Fail, c.explain(err, "")
		token.Fix = "set FACTORY_API_TOKEN to the token the server was started with"
		return []doctor.Result{reach, token, {Name: "server checks", Status: doctor.Skip, Detail: "not checked: the token was refused"}}
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		return []doctor.Result{reach, token, {
			Name: "server checks", Status: doctor.Fail, Detail: "the server has no doctor endpoint: it is older than this factory (" + version + ")",
			Fix: "upgrade the server to this factory's version",
		}}
	case err != nil:
		return []doctor.Result{reach, token, {
			Name: "server checks", Status: doctor.Fail, Detail: c.explain(err, ""),
			Fix: "read the server's log for the request, then run factory doctor again",
		}}
	}
	return append([]doctor.Result{reach, token, doctor.Version(version, answer.Version)}, answer.Checks...)
}

// workerDoctor checks this machine as a worker image.
func workerDoctor(ctx context.Context, s streams, output, workDir, harnesses string) int {
	if output != "table" && output != "json" {
		return usageError(s, "doctor", fmt.Sprintf("--output %q: use table or json", output))
	}
	workDir = firstSet(workDir, os.Getenv(envName("work-dir")), filepath.Join(os.TempDir(), "factory-worker"))
	chosen, err := pickHarnesses(firstSet(harnesses, os.Getenv(envName("harnesses")), "claude-code,grok"))
	if err != nil {
		return usageError(s, "doctor", err.Error())
	}
	results := []doctor.Result{doctor.Git(ctx)}
	results = append(results, doctor.Harnesses(ctx, chosen)...)
	results = append(results, doctor.Workspace(workDir))
	return report(ctx, s, output == "json", results, nil)
}

func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// report prints the results, offers a fix on a terminal, and returns 1 when
// any check failed.
func report(ctx context.Context, s streams, asJSON bool, results []doctor.Result, unchecked []string) int {
	failed := doctor.Failed(results)
	if asJSON {
		if code := printJSON(s, map[string]any{"checks": results, "failed": append([]string{}, failed...), "not_checked": append([]string{}, unchecked...)}); code != 0 {
			return code
		}
	} else {
		t := newTable(s.out)
		for _, r := range results {
			fmt.Fprintf(t, "%s\t%s\t%s\n", r.Status, r.Name, r.Detail)
			if r.Status != doctor.Pass && r.Fix != "" {
				fmt.Fprintf(t, "\t  fix:\t%s\n", r.Fix)
			}
		}
		_ = t.Flush()
		for i, u := range unchecked {
			if i == 0 {
				fmt.Fprintln(s.out, "\nNot checked yet, because the feature has not landed:")
			}
			fmt.Fprintf(s.out, "  %s\n", u)
		}
		offerFixes(ctx, s, results)
	}
	if len(failed) == 0 {
		if !asJSON {
			fmt.Fprintln(s.out, "\nEvery check passed.")
		}
		return 0
	}
	fmt.Fprintf(s.err, "factory doctor: %d check(s) failed: %s. Fix each as shown, then run 'factory doctor' again.\n",
		len(failed), strings.Join(failed, ", "))
	return 1
}

// offerFixes asks, on a terminal only, whether to run each failed check's
// fixing command, and runs it on yes.
func offerFixes(ctx context.Context, s streams, results []doctor.Result) {
	if !s.interactive {
		return
	}
	answers := bufio.NewReader(s.in)
	for _, r := range results {
		args, ok := strings.CutPrefix(r.Command, "factory ")
		if r.Status != doctor.Fail || !ok || !isCommand(strings.Fields(args)) {
			continue
		}
		fmt.Fprintf(s.out, "\n%s: run '%s' now? [Y/n] ", r.Name, r.Command)
		answer, _ := answers.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "" && a != "y" && a != "yes" {
			continue
		}
		dispatch(ctx, strings.Fields(args), s)
	}
}

// isCommand reports whether args name exactly one command of the table.
func isCommand(args []string) bool {
	for _, c := range commands() {
		if c.name == strings.Join(args, " ") {
			return true
		}
	}
	return false
}
