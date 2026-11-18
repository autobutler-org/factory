package work

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/lease"
)

// ErrRepoNotAllowed is returned when a worker asks for a code host token for
// a repository its claim does not cover.
var ErrRepoNotAllowed = errors.New("the worker's claim does not cover that repository")

// RepoToken mints a code host token for repo with mint, but only for a worker
// whose claim covers repo:
//
//   - a live claim on a step of item id's current graph whose params name
//     repo (a cluster worker), or
//   - a live claim on item id itself (a developer's local claim) when the
//     item's repos contain repo.
//
// Repositories are compared without regard to case, and the token is minted
// for the repository as the step or item spells it. Nothing is minted when
// the check fails.
func (s *Service) RepoToken(ctx context.Context, id, workerID, repo string, mint func(context.Context, string) (codehost.Token, error)) (codehost.Token, error) {
	if repo == "" {
		return codehost.Token{}, fmt.Errorf("%w: repo is required", ErrInvalid)
	}
	allowed, err := s.authorizeRepo(ctx, id, workerID, repo)
	if err != nil {
		return codehost.Token{}, fmt.Errorf("work item %s, worker %s, repo %s: %w", id, workerID, repo, err)
	}
	token, err := mint(ctx, allowed)
	if err != nil {
		return codehost.Token{}, fmt.Errorf("mint a token for %s: %w", allowed, err)
	}
	return token, nil
}

// authorizeRepo returns repo as the worker's claim spells it, or why the
// claim does not cover it.
func (s *Service) authorizeRepo(ctx context.Context, id, workerID, repo string) (string, error) {
	item, err := s.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if !uuidPattern.MatchString(workerID) {
		return "", lease.ErrNotClaimant
	}
	q := db.New(s.pool)
	if item.ClaimedBy != nil && *item.ClaimedBy == workerID {
		live, err := q.LeaseLive(ctx, id)
		if err != nil {
			return "", fmt.Errorf("read lease: %w", err)
		}
		if err := lease.Check(item.ClaimedBy, workerID, live); err != nil {
			return "", err
		}
		for _, r := range item.Repos {
			if strings.EqualFold(r, repo) {
				return r, nil
			}
		}
		return "", ErrRepoNotAllowed
	}
	held, err := q.HeldStepRepos(ctx, db.HeldStepReposParams{WorkItemID: id, WorkerID: &workerID})
	if err != nil {
		return "", fmt.Errorf("read held steps: %w", err)
	}
	return stepRepo(held, repo)
}

// stepRepo decides from the steps a worker holds on an item: a live step for
// repo allows it; only expired ones is lease.ErrExpired; steps for other
// repositories is ErrRepoNotAllowed; none is lease.ErrNotClaimant.
func stepRepo(held []db.HeldStepReposRow, repo string) (string, error) {
	if len(held) == 0 {
		return "", lease.ErrNotClaimant
	}
	expired := false
	for _, h := range held {
		if !strings.EqualFold(h.Repo, repo) {
			continue
		}
		if h.Live {
			return h.Repo, nil
		}
		expired = true
	}
	if expired {
		return "", lease.ErrExpired
	}
	return "", ErrRepoNotAllowed
}
