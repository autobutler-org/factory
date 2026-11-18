package codehost

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// PullBody is the description of a pull request opened in repo
// ("owner/name") for a work item. It is summary, then a link to the item's
// source when externalURL is set, then "Closes #N" when the source is issue N
// of repo on github.com, and last the other pull requests opened for the
// same item. Work from any other tracker is only linked.
func PullBody(repo, summary, externalURL string, siblings []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(summary))
	b.WriteString("\n")
	if externalURL != "" {
		fmt.Fprintf(&b, "\nSource: %s\n", externalURL)
		if n, ok := sameRepoIssue(repo, externalURL); ok {
			fmt.Fprintf(&b, "\nCloses #%d\n", n)
		}
	}
	if len(siblings) > 0 {
		b.WriteString("\nOther pull requests for the same work:\n")
		for _, s := range siblings {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	return b.String()
}

// sameRepoIssue returns N when rawURL is https://github.com/<repo>/issues/N.
func sameRepoIssue(repo, rawURL string) (int, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Host, "github.com") {
		return 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "issues" || !strings.EqualFold(parts[0]+"/"+parts[1], repo) {
		return 0, false
	}
	n, err := strconv.Atoi(parts[3])
	return n, err == nil && n > 0
}

// PullNumber is the pull request number at the end of a pull request's html
// URL, such as a URL recorded as a work output.
func PullNumber(prURL string) (int, error) {
	u, err := url.Parse(prURL)
	if err != nil {
		return 0, fmt.Errorf("pull request URL %q: %w", prURL, err)
	}
	if path.Base(path.Dir(strings.TrimSuffix(u.Path, "/"))) != "pull" {
		return 0, fmt.Errorf("pull request URL %q has no pull number", prURL)
	}
	n, err := strconv.Atoi(path.Base(u.Path))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("pull request URL %q has no pull number", prURL)
	}
	return n, nil
}
