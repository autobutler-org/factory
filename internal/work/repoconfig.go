package work

import (
	"context"
	"errors"
	"fmt"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/plan"
	"github.com/autobutler-org/factory/internal/repoconfig"
	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/jackc/pgx/v5"
)

// EventPlanDeferred records that an item stayed ready because its
// repositories' configs could not be read.
const EventPlanDeferred = "plan_deferred"

var (
	// ErrBadRepoConfig is returned when a repository's config cannot be
	// planned with: it is missing or invalid, or names an image the server
	// does not allow. Retrying does not help; the item is blocked.
	ErrBadRepoConfig = errors.New("repository config cannot be used")
	// ErrConfigUnavailable is returned when the code host could not be
	// read. It is retryable: the item stays ready.
	ErrConfigUnavailable = errors.New("repository config could not be read")
)

// Configs is how the server reads each repository's .factory/config.toml
// when it plans an item.
type Configs struct {
	// Host returns the code host for a repository, holding a token for it.
	Host func(ctx context.Context, repo string) (codehost.Host, error)
	// AllowedImages are the image repository prefixes a config's image may
	// name (sandbox.AllowImage). Empty allows no image.
	AllowedImages []string
}

// configRead is what was read for one repository, recorded on the planned
// event.
type configRead struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Commit string `json:"commit"`
	Image  string `json:"image,omitempty"`
}

// WithConfigs returns a copy of s that plans each item with its
// repositories' configs, read through c. Without it every repository is
// planned with plan.DefaultRepoConfig.
func (s *Service) WithConfigs(c Configs) *Service {
	out := *s
	out.configs = &c
	return &out
}

// readConfigs reads and checks the config of each repository at its default
// branch. With no Configs it returns none, and the planner uses defaults.
func (s *Service) readConfigs(ctx context.Context, repos []string) (map[string]plan.RepoConfig, []configRead, error) {
	if s.configs == nil {
		return nil, nil, nil
	}
	configs := make(map[string]plan.RepoConfig, len(repos))
	reads := make([]configRead, 0, len(repos))
	for _, repo := range repos {
		cfg, read, err := s.configs.read(ctx, repo)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", repo, err)
		}
		configs[repo] = cfg.Plan()
		reads = append(reads, read)
	}
	return configs, reads, nil
}

func (c Configs) read(ctx context.Context, repo string) (repoconfig.Config, configRead, error) {
	host, err := c.Host(ctx, repo)
	if err != nil {
		return repoconfig.Config{}, configRead{}, fmt.Errorf("%w: %w", ErrConfigUnavailable, err)
	}
	branch, err := host.DefaultBranch(ctx)
	if err != nil {
		return repoconfig.Config{}, configRead{}, fmt.Errorf("%w: read the default branch: %w", ErrConfigUnavailable, err)
	}
	f, err := host.ReadFile(ctx, branch, repoconfig.Path)
	if errors.Is(err, codehost.ErrFileNotFound) {
		return repoconfig.Config{}, configRead{}, fmt.Errorf("%w: %s not found on %s; add it with at least gate = \"<command>\" (docs/config.md)", ErrBadRepoConfig, repoconfig.Path, branch)
	}
	if err != nil {
		return repoconfig.Config{}, configRead{}, fmt.Errorf("%w: read %s: %w", ErrConfigUnavailable, repoconfig.Path, err)
	}
	cfg, err := repoconfig.Parse(repoconfig.Path+" at "+f.Commit, f.Content)
	if err != nil {
		return repoconfig.Config{}, configRead{}, fmt.Errorf("%w: %w", ErrBadRepoConfig, err)
	}
	if cfg.Image != "" {
		if err := sandbox.AllowImage(cfg.Image, c.AllowedImages); err != nil {
			return repoconfig.Config{}, configRead{}, fmt.Errorf("%w: %w", ErrBadRepoConfig, err)
		}
	}
	return cfg, configRead{Repo: repo, Branch: branch, Commit: f.Commit, Image: cfg.Image}, nil
}

// SandboxImage returns the image the chain with the given sandbox key runs in, as
// its current graph records it, or "" for the launcher's default (also for
// a key with no current graph).
func (s *Service) SandboxImage(ctx context.Context, key string) (string, error) {
	image, err := db.New(s.pool).SandboxImage(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the image of %s: %w", key, err)
	}
	return image, nil
}
