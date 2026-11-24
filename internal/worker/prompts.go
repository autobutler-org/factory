package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/autobutler-org/factory/internal/repoconfig"
)

// Ported from schmeide's brief, reviewBrief and fixBrief. The prompts say
// where the work came from with the item's source and external URL; they
// assume no tracker.

// Files a session leaves for the worker, relative to the checkout. They are
// removed before anything is committed.
const (
	prTextPath     = ".factory/pr.md"
	reviewTextPath = ".factory/review.md"
	blockedPath    = ".factory/blocked.md"
	// gateOutputPath is where a session that repairs a failed gate finds
	// the gate's whole output. The worker writes it; nobody commits it.
	gateOutputPath = ".factory/gate-output.txt"
)

// reviewLead is the first sentence of a review prompt. No other prompt
// starts this way, so a fake agent can tell the sessions apart.
const reviewLead = "You are reviewing the change for a pull request that is already open."

// gateFixLead is the first sentence of a fix-gate prompt, for a fake agent
// to tell it apart as it does a review.
const gateFixLead = "You are working on a factory work item, and the repository's gate command failed on its branch."

// task describes the work item for a prompt.
func task(item Item, repo string) string {
	origin := "It was created directly in the factory."
	switch {
	case item.ExternalURL != "" && item.Source != "":
		origin = fmt.Sprintf("It came from %s: %s", item.Source, item.ExternalURL)
	case item.ExternalURL != "":
		origin = "It came from " + item.ExternalURL
	case item.Source != "":
		origin = "It came from " + item.Source + "."
	}
	return fmt.Sprintf(`The work item is for the repository %s. %s

Title: %s

Description:
%s
`, repo, origin, item.Title, strings.TrimSpace(item.Body))
}

// blockedNote tells a writing session how to stop for a decision.
const blockedNote = `If you cannot proceed without a decision from a person, make no change and write the question to ` + blockedPath + ` instead.`

// brief is the prompt for the implement session.
func brief(item Item, repo, gate string) string {
	return fmt.Sprintf(`You are working on a factory work item, in a checkout of the repository on its own branch.

%s
Make the change the work item asks for, with tests where the repository expects them. Follow the repository's own conventions (README, CONTRIBUTING, agent instruction files). Stay inside this directory. Do not commit, push or open a pull request: that is done for you once the repository's gate command passes. The gate command is: %s

When you are done, write the pull request text to %s: the title on the first line, following the repository's commit and pull request title conventions, then a blank line, then the body, following the repository's pull request template if it has one. Put no AI attribution in it: no generated-by footer, no co-author trailer for an AI, and no session link.

%s
`, task(item, repo), gate, prTextPath, blockedNote)
}

// reviewBrief is the prompt for one review session. The verdict contract is
// the file at reviewTextPath, so no session output is parsed. gateChanged
// are the files that define the gate a session asked to make it pass
// changed.
func reviewBrief(item Item, repo string, gateChanged []string) string {
	gate := ""
	if len(gateChanged) > 0 {
		gate = fmt.Sprintf("\nA session asked to make the repository's gate pass changed these files, which define the gate: %s. Check that the change does not weaken the gate (fewer or looser checks, skipped tests, a command that cannot fail); if it does, request changes.\n", strings.Join(gateChanged, ", "))
	}
	return fmt.Sprintf(`%s

%s%s
Review the change on this branch against the work item: compare it with its base and read the result. Do not edit the code, do not commit, and do not push. Write your verdict to %s and write nothing else.
- If the change does what the work item asks, the file's only line is: pass
- If it does not, the first line is "changes" and the rest of the file is what must be fixed.

The first line must be "pass" or "changes".
`, reviewLead, task(item, repo), gate, reviewTextPath)
}

// fixBrief is the prompt for a session that applies review findings.
func fixBrief(item Item, repo, gate, findings string) string {
	return fmt.Sprintf(`You are working on a factory work item. A reviewer asked for changes on its open pull request.

%s
Reviewer findings:
%s

Apply those findings, with tests where the repository expects them. Follow the repository's own conventions. Stay inside this directory. Do not commit, push or open a pull request: the open pull request updates once the gate passes. The gate command is: %s

If a finding is about the pull request's title or description (a failed check may read them), write the whole new pull request text to %s: the title on the first line, following the repository's commit and pull request title conventions, then a blank line, then the body, following the repository's pull request template if it has one. The current text is the message of the branch's first commit after its base. The open pull request is updated from that file; rewriting it counts as a change, so no code change is needed for such a finding. Put no AI attribution in it: no generated-by footer, no co-author trailer for an AI, and no session link.

%s
`, task(item, repo), strings.TrimSpace(findings), gate, prTextPath, blockedNote)
}

// gateFixBrief is the prompt for a session that makes a failed gate pass
// after a fix session's change. reason is the gate's failure reason; the
// whole output is in gateOutputPath.
func gateFixBrief(item Item, repo, gate, reason string) string {
	return fmt.Sprintf(`You are working on a factory work item. A change for its open pull request was just committed on this branch, and the repository's gate command failed on it.

%s
The gate command is: %s

How it failed:
%s

The gate's whole output is in %s (cut to its first and last lines when it was larger than 5 MiB). Read it to find what failed.

Make the gate pass, with the smallest change that fixes the cause, and keep what the change was for. Follow the repository's own conventions. Stay inside this directory. Do not commit, push or open a pull request: the open pull request updates once the gate passes.

%s
`, task(item, repo), gate, strings.TrimSpace(reason), gateOutputPath, blockedNote)
}

// fixGateBrief is the prompt for a fix-gate session: a gate round failed
// on the change and the next round runs the gate again. failure is the
// gate's failure reason, which quotes the failing lines and the end of the
// output.
func fixGateBrief(item Item, repo, gate, failure string) string {
	return fmt.Sprintf(`%s

%s
The gate command is: %s

How it failed:
%s

Make the gate pass, with the smallest change that fixes the cause, and keep what the change was for. Do not weaken the gate: do not loosen the checks it runs to make them pass more easily. Change %s or the files the gate command names only when the fix truly needs it; such a change is flagged on the pull request for review and a person must approve it. Follow the repository's own conventions. Stay inside this directory. Do not commit, push or open a pull request: the gate runs again once you are done.

%s
`, gateFixLead, task(item, repo), gate, strings.TrimSpace(failure), repoconfig.Path, blockedNote)
}

// readNote reads and removes the file a session left at rel. ok is false
// when there is none.
func readNote(dir, rel string) (text string, ok bool) {
	file := filepath.Join(dir, filepath.FromSlash(rel))
	data, err := os.ReadFile(file)
	if err != nil {
		return "", false
	}
	_ = os.Remove(file)
	return strings.TrimSpace(string(data)), true
}

// dropNotes removes every file a session uses to talk to the worker, so a
// commit does not record them.
func dropNotes(dir string) {
	for _, rel := range []string{prTextPath, reviewTextPath, blockedPath, gateOutputPath} {
		_ = os.Remove(filepath.Join(dir, filepath.FromSlash(rel)))
	}
}

// prText is the title and body the session left in prTextPath, or the item's
// title when it left none. ok says whether it left the file.
func prText(dir string, item Item) (title, body string, ok bool) {
	text, ok := readNote(dir, prTextPath)
	title, body, _ = strings.Cut(text, "\n")
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if title == "" {
		title = item.Title
	}
	return title, body, ok
}

// readVerdict reads and removes the reviewer's verdict file.
func readVerdict(dir string) (verdict, findings string, err error) {
	text, ok := readNote(dir, reviewTextPath)
	if !ok {
		return "", "", fmt.Errorf("the reviewer did not write %s with a first line of \"pass\" or \"changes\"", reviewTextPath)
	}
	line, rest, _ := strings.Cut(text, "\n")
	line, rest = strings.TrimSpace(line), strings.TrimSpace(rest)
	switch line {
	case "pass":
		return "pass", "", nil
	case "changes":
		if rest == "" {
			return "", "", errors.New("the reviewer requested changes but did not say what to fix")
		}
		return "changes", rest, nil
	}
	return "", "", fmt.Errorf("the reviewer wrote %q as its verdict; the first line of %s must be \"pass\" or \"changes\"", line, reviewTextPath)
}
