package codehost

import "testing"

func TestPullBody(t *testing.T) {
	const repo = "autobutler-org/quark"
	for _, tc := range []struct {
		name        string
		externalURL string
		siblings    []string
		want        string
	}{
		{
			name:        "github issue in the same repository closes it",
			externalURL: "https://github.com/autobutler-org/quark/issues/123",
			want:        "Fix it.\n\nSource: https://github.com/autobutler-org/quark/issues/123\n\nCloses #123\n",
		},
		{
			name:        "repository match ignores case",
			externalURL: "https://github.com/Autobutler-Org/Quark/issues/7",
			want:        "Fix it.\n\nSource: https://github.com/Autobutler-Org/Quark/issues/7\n\nCloses #7\n",
		},
		{
			name:        "github issue in another repository only links",
			externalURL: "https://github.com/autobutler-org/infra/issues/123",
			want:        "Fix it.\n\nSource: https://github.com/autobutler-org/infra/issues/123\n",
		},
		{
			name:        "a github pull request is not closed",
			externalURL: "https://github.com/autobutler-org/quark/pull/9",
			want:        "Fix it.\n\nSource: https://github.com/autobutler-org/quark/pull/9\n",
		},
		{
			name:        "jira source only links",
			externalURL: "https://autobutler.atlassian.net/browse/FAC-42",
			want:        "Fix it.\n\nSource: https://autobutler.atlassian.net/browse/FAC-42\n",
		},
		{
			name: "no source",
			want: "Fix it.\n",
		},
		{
			name:        "siblings are cross-linked",
			externalURL: "https://autobutler.atlassian.net/browse/FAC-42",
			siblings:    []string{"https://github.com/autobutler-org/infra/pull/4", "https://github.com/autobutler-org/web/pull/8"},
			want: "Fix it.\n\nSource: https://autobutler.atlassian.net/browse/FAC-42\n\n" +
				"Other pull requests for the same work:\n- https://github.com/autobutler-org/infra/pull/4\n- https://github.com/autobutler-org/web/pull/8\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PullBody(repo, "Fix it.", tc.externalURL, tc.siblings); got != tc.want {
				t.Errorf("PullBody =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestPullNumber(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want int
		ok   bool
	}{
		{"https://github.com/o/r/pull/12", 12, true},
		{"https://github.com/o/r/pull/12/", 12, true},
		{"https://github.com/o/r/pull/0", 0, false},
		{"https://github.com/o/r/pulls", 0, false},
		{"https://github.com/o/r/issues/12", 0, false},
		{"://bad", 0, false},
	} {
		got, err := PullNumber(tc.url)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("PullNumber(%q) = %d, %v; want %d, ok %v", tc.url, got, err, tc.want, tc.ok)
		}
	}
}
