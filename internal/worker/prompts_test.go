package worker

import (
	"strings"
	"testing"
)

// The sessions write the text the worker commits and opens the pull request
// with, so each prompt that asks for such text forbids AI attribution in it.
func TestPromptsForbidAttribution(t *testing.T) {
	t.Parallel()
	item := Item{Title: "a title", Body: "a body"}
	for name, prompt := range map[string]string{
		"brief": brief(item, "owner/repo", "make check"),
	} {
		if !strings.Contains(prompt, "no AI attribution") {
			t.Errorf("%s prompt does not forbid AI attribution:\n%s", name, prompt)
		}
	}
}
