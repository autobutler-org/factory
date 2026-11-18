package work_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/work"
)

func recordingMint(minted *[]string) func(context.Context, string) (codehost.Token, error) {
	return func(_ context.Context, repo string) (codehost.Token, error) {
		*minted = append(*minted, repo)
		return codehost.Token{Token: "ghs_" + repo, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
}

// TestRepoTokenForAStepClaim mints a token for repository R only for a
// worker holding a live claim on a step of the item whose params name R.
func TestRepoTokenForAStepClaim(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	a, b, idle := register(t, svc, "a"), register(t, svc, "b"), register(t, svc, "idle")
	item := mustCreate(t, svc, work.NewItem{Title: "x", Repos: []string{"o/a", "o/b"}})
	other := mustCreate(t, svc, work.NewItem{Title: "y", Repos: []string{"o/b"}})
	stepA := mustClaim(t, svc, a.ID, "o/a/workspace")
	mustClaim(t, svc, b.ID, "o/b/workspace")
	var minted []string
	mint := recordingMint(&minted)

	for _, tc := range []struct {
		name, id, worker, repo string
	}{
		{"a for its step's repository", item.ID, a.ID, "o/a"},
		{"case does not matter", item.ID, a.ID, "O/A"},
		{"b for its step's repository", item.ID, b.ID, "o/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := svc.RepoToken(ctx, tc.id, tc.worker, tc.repo, mint); err != nil || got.Token == "" {
				t.Errorf("RepoToken = %+v, %v; want a token", got, err)
			}
		})
	}
	if minted[1] != "o/a" {
		t.Errorf("minted for %q, want the repository as the step names it", minted[1])
	}
	for _, tc := range []struct {
		name, id, worker, repo string
		want                   error
	}{
		{"a for the item's other repository", item.ID, a.ID, "o/b", work.ErrRepoNotAllowed},
		{"a for a repository the item does not name", item.ID, a.ID, "o/c", work.ErrRepoNotAllowed},
		{"a worker holding nothing", item.ID, idle.ID, "o/a", lease.ErrNotClaimant},
		{"no worker", item.ID, "", "o/a", lease.ErrNotClaimant},
		{"b for another item it holds no step of", other.ID, b.ID, "o/b", lease.ErrNotClaimant},
		{"no repository", item.ID, a.ID, "", work.ErrInvalid},
		{"no such item", "00000000-0000-0000-0000-000000000000", a.ID, "o/a", work.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.RepoToken(ctx, tc.id, tc.worker, tc.repo, mint); !errors.Is(err, tc.want) {
				t.Errorf("RepoToken = %v, want %v", err, tc.want)
			}
		})
	}

	// a's step lease runs out: refused before the reaper and after it.
	if _, err := pool.Exec(ctx, `UPDATE work_steps SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, stepA.Step.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RepoToken(ctx, item.ID, a.ID, "o/a", mint); !errors.Is(err, lease.ErrExpired) {
		t.Errorf("RepoToken with an expired step lease = %v, want ErrExpired", err)
	}
	if _, err := svc.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RepoToken(ctx, item.ID, a.ID, "o/a", mint); !errors.Is(err, lease.ErrNotClaimant) {
		t.Errorf("RepoToken after the reaper took the step = %v, want ErrNotClaimant", err)
	}
	if len(minted) != 3 {
		t.Errorf("minted for %v, want only the three authorized requests", minted)
	}
}

// TestRepoTokenForALocalClaim mints a token for a developer holding the
// whole item, for any of the item's repos.
func TestRepoTokenForALocalClaim(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	dev, err := svc.RegisterWorker(ctx, work.WorkerInput{Name: "laptop", Kind: work.WorkerLocal})
	if err != nil {
		t.Fatal(err)
	}
	other := register(t, svc, "other")
	item := mustCreate(t, svc, work.NewItem{Title: "x", Repos: []string{"o/a", "o/b"}})
	unclaimed := mustCreate(t, svc, work.NewItem{Title: "y", Repos: []string{"o/a"}})
	if _, err := svc.Claim(ctx, item.ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	var minted []string
	mint := recordingMint(&minted)

	got, err := svc.RepoToken(ctx, item.ID, dev.ID, "O/B", mint)
	if err != nil || got.Token != "ghs_o/b" {
		t.Fatalf("RepoToken for the holder = %+v, %v; want a token for o/b as the item spells it", got, err)
	}
	for _, tc := range []struct {
		name, id, worker, repo string
		want                   error
	}{
		{"another worker", item.ID, other.ID, "o/a", lease.ErrNotClaimant},
		{"a repository the item does not name", item.ID, dev.ID, "o/c", work.ErrRepoNotAllowed},
		{"an item nobody holds", unclaimed.ID, dev.ID, "o/a", lease.ErrNotClaimant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.RepoToken(ctx, tc.id, tc.worker, tc.repo, mint); !errors.Is(err, tc.want) {
				t.Errorf("RepoToken = %v, want %v", err, tc.want)
			}
		})
	}

	failing := func(context.Context, string) (codehost.Token, error) {
		return codehost.Token{}, errors.New("github down")
	}
	if _, err := svc.RepoToken(ctx, item.ID, dev.ID, "o/a", failing); err == nil || !strings.Contains(err.Error(), "github down") {
		t.Errorf("RepoToken with a failing minter = %v, want its error", err)
	}

	age(t, pool, item.ID, "lease_expires_at", time.Hour)
	if _, err := svc.RepoToken(ctx, item.ID, dev.ID, "o/a", mint); !errors.Is(err, lease.ErrExpired) {
		t.Errorf("RepoToken after the lease ran out = %v, want ErrExpired", err)
	}
	if len(minted) != 1 {
		t.Errorf("minted for %v, want only the one authorized request", minted)
	}
}

// TestJiraItemOpensLinkedPullRequestsInTwoRepos opens one pull request in
// each of a Jira-sourced item's two repositories, links them to each other
// and to the Jira issue, and records both as the item's outputs.
func TestJiraItemOpensLinkedPullRequestsInTwoRepos(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t)
	w := register(t, svc, "w")
	const jira = "https://autobutler.atlassian.net/browse/FAC-42"
	item := mustCreate(t, svc, work.NewItem{
		Source: "jira", ExternalID: "FAC-42", ExternalURL: jira,
		Title: "Rotate the staging certificates", Repos: []string{"o/a", "o/b"},
	})
	if _, err := svc.Claim(ctx, item.ID, w.ID); err != nil {
		t.Fatal(err)
	}
	branch := plan.BranchName(item.ID, item.Title)
	if !strings.HasPrefix(branch, strings.ReplaceAll(item.ID, "-", "")[:8]+"_rotate-the-staging") {
		t.Errorf("branch = %q", branch)
	}

	hosts := map[string]*codehost.Fake{"o/a": {Repo: "o/a"}, "o/b": {Repo: "o/b"}}
	var urls []string
	for _, repo := range item.Repos {
		f := hosts[repo]
		body := codehost.PullBody(repo, "Rotate them.", *item.ExternalUrl, urls)
		if strings.Contains(body, "Closes") || !strings.Contains(body, jira) {
			t.Errorf("body for %s = %q, want a Jira link and no Closes", repo, body)
		}
		p, err := f.OpenPull(ctx, item.Title, body, branch, "main")
		if err != nil {
			t.Fatal(err)
		}
		// Earlier siblings learn of this one by a comment.
		for _, u := range urls {
			n, err := codehost.PullNumber(u)
			if err != nil {
				t.Fatal(err)
			}
			if err := hosts["o/a"].Comment(ctx, n, "Also part of this work: "+p.HTMLURL); err != nil {
				t.Fatal(err)
			}
		}
		urls = append(urls, p.HTMLURL)
		if _, err := svc.RecordWorkerOutput(ctx, item.ID, w.ID, work.OutputInput{
			Repo: repo, Kind: work.KindPullRequest, Branch: branch, URL: p.HTMLURL, Status: work.OutputOpen,
		}); err != nil {
			t.Fatal(err)
		}
	}

	outputs, err := svc.Outputs(ctx, item.ID)
	if err != nil || len(outputs) != 2 || outputs[0].Repo == outputs[1].Repo {
		t.Fatalf("outputs = %+v, %v; want one per repository", outputs, err)
	}
	if got := hosts["o/a"].Comments()[1]; len(got) != 1 || !strings.Contains(got[0], urls[1]) {
		t.Errorf("o/a pull 1 comments = %q, want a link to %s", got, urls[1])
	}
}
