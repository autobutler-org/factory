package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
)

// reviewLead is the first sentence of a review prompt. No other prompt
// starts this way, so a fake agent can tell the sessions apart.
const reviewLead = "You are reviewing the change for a pull request that is already open."

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
// the file at reviewTextPath, so no session output is parsed.
func reviewBrief(item Item, repo string) string {
	return fmt.Sprintf(`%s

%s
Review the change on this branch against the work item: compare it with its base and read the result. Do not edit the code, do not commit, and do not push. Write your verdict to %s and write nothing else.
- If the change does what the work item asks, the file's only line is: pass
- If it does not, the first line is "changes" and the rest of the file is what must be fixed.

The first line must be "pass" or "changes".
`, reviewLead, task(item, repo), reviewTextPath)
}

// fixBrief is the prompt for a session that applies review findings.
func fixBrief(item Item, repo, gate, findings string) string {
	return fmt.Sprintf(`You are working on a factory work item. A reviewer asked for changes on its open pull request.

%s
Reviewer findings:
%s

Apply those findings, with tests where the repository expects them. Follow the repository's own conventions. Stay inside this directory. Do not commit, push or open a pull request: the open pull request updates once the gate passes. The gate command is: %s

%s
`, task(item, repo), strings.TrimSpace(findings), gate, blockedNote)
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
	for _, rel := range []string{prTextPath, reviewTextPath, blockedPath} {
		_ = os.Remove(filepath.Join(dir, filepath.FromSlash(rel)))
	}
}

// prText is the title and body the session left in prTextPath, or the item's
// title when it left none.
func prText(dir string, item Item) (title, body string) {
	text, _ := readNote(dir, prTextPath)
	title, body, _ = strings.Cut(text, "\n")
	title, body = strings.TrimSpace(title), strings.TrimSpace(body)
	if title == "" {
		title = item.Title
	}
	return title, body
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
