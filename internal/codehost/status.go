package codehost

import (
	"context"
	"slices"
	"strings"
	"time"
)

// Pull request states PullStatus reports.
const (
	PullOpen   = "open"
	PullClosed = "closed" // closed without merging
	PullMerged = "merged"
)

// Check states PullStatus reports.
const (
	CheckPending = "pending"
	CheckPassed  = "passed"
	CheckFailed  = "failed"
)

// PullStatus is what the server's poller reads about one pull request.
type PullStatus struct {
	// State is PullOpen, PullClosed or PullMerged.
	State string
	// Head is the SHA of the pull request's head commit, and HeadCommitted
	// its commit date. Checks are that commit's only.
	Head          string
	HeadCommitted time.Time
	// Approved is true when the repository's review rules are met, or, on a
	// branch with no review rule, when a review approves and none of the
	// latest reviews requests changes.
	Approved bool
	// Required are the check names the base branch requires, from branch
	// protection and rulesets. Empty means none is required, or protection
	// could not be read.
	Required []string
	// Checks are the head commit's check runs and commit statuses, in the
	// order GitHub lists them.
	Checks []Check
}

// Check is one check run or commit status on a commit, by its name (a
// status's context).
type Check struct {
	Name string
	// State is CheckPending, CheckPassed or CheckFailed.
	State string
}

// pullStatusQuery reads a pull request's state, reviews, the base branch's
// required checks, and the head commit's check runs and statuses in one
// GraphQL request, which costs one point of the GraphQL rate limit.
//
// ponytail: the first 100 checks and rules; page contexts if a head ever
// carries more.
const pullStatusQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      state
      reviewDecision
      latestOpinionatedReviews(first: 100) { nodes { state } }
      baseRef {
        branchProtectionRule { requiredStatusCheckContexts }
        rules(first: 100) { nodes { type parameters { ... on RequiredStatusChecksParameters { requiredStatusChecks { context } } } } }
      }
      commits(last: 1) { nodes { commit { oid committedDate statusCheckRollup { contexts(first: 100) { nodes {
        __typename
        ... on CheckRun { name status conclusion }
        ... on StatusContext { context state }
      } } } } } }
    }
  }
}`

// PullStatus reads pull request number with one GraphQL query. Checks are
// both kinds GitHub has: check runs (GitHub Actions and other Apps) and
// commit statuses (older integrations).
func (g GitHub) PullStatus(ctx context.Context, number int) (PullStatus, error) {
	owner, name, _ := strings.Cut(g.Repo, "/")
	var resp struct {
		Repository struct {
			PullRequest struct {
				State          string `json:"state"`
				ReviewDecision string `json:"reviewDecision"`
				Reviews        struct {
					Nodes []struct {
						State string `json:"state"`
					} `json:"nodes"`
				} `json:"latestOpinionatedReviews"`
				BaseRef *struct {
					Protection *struct {
						Contexts []string `json:"requiredStatusCheckContexts"`
					} `json:"branchProtectionRule"`
					Rules *struct {
						Nodes []struct {
							Type       string `json:"type"`
							Parameters *struct {
								Checks []struct {
									Context string `json:"context"`
								} `json:"requiredStatusChecks"`
							} `json:"parameters"`
						} `json:"nodes"`
					} `json:"rules"`
				} `json:"baseRef"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							OID       string    `json:"oid"`
							Committed time.Time `json:"committedDate"`
							Rollup    *struct {
								Contexts struct {
									Nodes []struct {
										Type       string `json:"__typename"`
										Name       string `json:"name"`
										Status     string `json:"status"`
										Conclusion string `json:"conclusion"`
										Context    string `json:"context"`
										State      string `json:"state"`
									} `json:"nodes"`
								} `json:"contexts"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := g.graphql(ctx, pullStatusQuery, map[string]any{"owner": owner, "name": name, "number": number}, &resp); err != nil {
		return PullStatus{}, err
	}
	pr := resp.Repository.PullRequest
	st := PullStatus{State: strings.ToLower(pr.State)}
	reviews := make([]string, 0, len(pr.Reviews.Nodes))
	for _, r := range pr.Reviews.Nodes {
		reviews = append(reviews, r.State)
	}
	st.Approved = approved(pr.ReviewDecision, reviews)
	if b := pr.BaseRef; b != nil {
		if b.Protection != nil {
			st.Required = append(st.Required, b.Protection.Contexts...)
		}
		if b.Rules != nil {
			for _, rule := range b.Rules.Nodes {
				if rule.Type != "REQUIRED_STATUS_CHECKS" || rule.Parameters == nil {
					continue
				}
				for _, c := range rule.Parameters.Checks {
					if !slices.Contains(st.Required, c.Context) {
						st.Required = append(st.Required, c.Context)
					}
				}
			}
		}
	}
	if len(pr.Commits.Nodes) == 0 {
		return st, nil
	}
	head := pr.Commits.Nodes[0].Commit
	st.Head, st.HeadCommitted = head.OID, head.Committed
	if head.Rollup == nil {
		return st, nil
	}
	for _, c := range head.Rollup.Contexts.Nodes {
		if c.Type == "StatusContext" {
			st.Checks = append(st.Checks, Check{Name: c.Context, State: statusState(c.State)})
		} else {
			st.Checks = append(st.Checks, Check{Name: c.Name, State: checkRunState(c.Status, c.Conclusion)})
		}
	}
	return st, nil
}

// approved applies GitHub's review decision, which is empty when the branch
// has no review rule; then one approval with no change request is enough.
func approved(decision string, reviews []string) bool {
	if decision != "" {
		return decision == "APPROVED"
	}
	return slices.Contains(reviews, "APPROVED") && !slices.Contains(reviews, "CHANGES_REQUESTED")
}

// checkRunState: a run that has not completed is pending; success, neutral
// and skipped pass, as GitHub counts them for a required check; every other
// conclusion (failure, cancelled, timed out, action required, stale, startup
// failure) fails.
func checkRunState(status, conclusion string) string {
	switch {
	case !strings.EqualFold(status, "COMPLETED"):
		return CheckPending
	case strings.EqualFold(conclusion, "SUCCESS"), strings.EqualFold(conclusion, "NEUTRAL"), strings.EqualFold(conclusion, "SKIPPED"):
		return CheckPassed
	}
	return CheckFailed
}

// statusState maps a commit status: pending and expected are pending,
// failure and error fail.
func statusState(state string) string {
	switch strings.ToUpper(state) {
	case "SUCCESS":
		return CheckPassed
	case "FAILURE", "ERROR":
		return CheckFailed
	}
	return CheckPending
}
