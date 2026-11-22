package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/launcher"
	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/autobutler-org/factory/internal/work"
	"github.com/jackc/pgx/v5/pgxpool"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// launcherInterval is how often the launcher passes.
const launcherInterval = 10 * time.Second

// checksGrace is how long an await_checks step waits on a head commit with
// no checks reported before it passes, for a repository without CI. CI that
// reports at all reports a queued check within seconds of a push.
const checksGrace = 2 * time.Minute

// launcherFlags are factory serve's launcher settings, as parsed.
type launcherFlags struct {
	enabled                                   *bool
	namespace, image, serverURL               *string
	allowedImages                             *string
	maxSandboxes                              *int
	pendingTimeout, pollInterval              *time.Duration
	capabilities, serviceAccount              *string
	nodeSelector, tolerations                 *string
	requests, limits                          *string
	apiTokenSecret, modelKeySecret, runtimeCl *string
}

func addLauncherFlags(fs *flag.FlagSet) launcherFlags {
	return launcherFlags{
		enabled:        fs.Bool("launcher", false, "run the launcher and the server-side steps"),
		namespace:      fs.String("launcher-namespace", "factory-workers", "`namespace` the worker Jobs run in"),
		image:          fs.String("launcher-image", "", "worker `image`; required with --launcher"),
		serverURL:      fs.String("launcher-server-url", "", "`URL` workers reach this server at; required with --launcher"),
		allowedImages:  fs.String("launcher-allowed-images", "", "comma-separated image repository prefixes, such as ghcr.io/autobutler-org/, a repository's image (image in .factory/config.toml) may come from; a prefix without a trailing / is one repository. Empty lets no repository choose its image"),
		maxSandboxes:   fs.Int("launcher-max-sandboxes", 4, "most worker sandboxes alive at once"),
		pendingTimeout: fs.Duration("launcher-pending-timeout", 10*time.Minute, "destroy a sandbox still pending after this long (an image that will not pull, a pod that cannot be scheduled) and start a new one; 0 never does"),
		pollInterval:   fs.Duration("launcher-poll-interval", time.Minute, "how often the launcher reads each open pull request of the items being worked from the code host, to complete wait steps and record merges; one GraphQL request per pull request"),
		capabilities:   fs.String("launcher-capabilities", "claude-code", "comma-separated capabilities the worker image registers; items requiring others are left alone"),
		serviceAccount: fs.String("launcher-service-account", "factory-worker", "ServiceAccount `name` of worker pods"),
		nodeSelector:   fs.String("launcher-node-selector", "", "key=value,... node selector for worker pods"),
		tolerations:    fs.String("launcher-tolerations", "", "key=value:Effect or key:Effect,... tolerations for worker pods"),
		requests:       fs.String("launcher-requests", "cpu=1,memory=2Gi,ephemeral-storage=10Gi", "cpu, memory and ephemeral-storage requests of the worker container"),
		limits:         fs.String("launcher-limits", "cpu=4,memory=8Gi,ephemeral-storage=20Gi", "cpu, memory and ephemeral-storage limits of the worker container"),
		apiTokenSecret: fs.String("launcher-api-token-secret", "factory-worker-api-token", "Secret `name` in the worker namespace whose key \"token\" is given to workers as FACTORY_API_TOKEN"),
		modelKeySecret: fs.String("launcher-model-key-secret", "factory-model-api-key", "Secret `name` in the worker namespace whose key \"api-key\" is given to workers as ANTHROPIC_API_KEY"),
		runtimeCl:      fs.String("launcher-runtime-class", "", "RuntimeClass `name` of worker pods, such as gvisor"),
	}
}

// jobsConfig checks the settings and turns them into the Job template's
// configuration. Its errors are usage errors.
func (f launcherFlags) jobsConfig() (sandbox.JobsConfig, error) {
	nodeSelector, err := sandbox.ParseLabels(*f.nodeSelector)
	if err != nil {
		return sandbox.JobsConfig{}, fmt.Errorf("--launcher-node-selector: %w", err)
	}
	tolerations, err := sandbox.ParseTolerations(*f.tolerations)
	if err != nil {
		return sandbox.JobsConfig{}, fmt.Errorf("--launcher-tolerations: %w", err)
	}
	requests, err := sandbox.ParseResources(*f.requests)
	if err != nil {
		return sandbox.JobsConfig{}, fmt.Errorf("--launcher-requests: %w", err)
	}
	limits, err := sandbox.ParseResources(*f.limits)
	if err != nil {
		return sandbox.JobsConfig{}, fmt.Errorf("--launcher-limits: %w", err)
	}
	if *f.enabled {
		switch {
		case *f.image == "":
			return sandbox.JobsConfig{}, errors.New("--launcher needs --launcher-image, the worker image to run")
		case *f.serverURL == "":
			return sandbox.JobsConfig{}, errors.New("--launcher needs --launcher-server-url, the URL workers reach this server at")
		case *f.pollInterval < launcherInterval:
			return sandbox.JobsConfig{}, fmt.Errorf("--launcher-poll-interval must be at least %s, the launcher's pass; each poll costs one GitHub request per open pull request", launcherInterval)
		case *f.pendingTimeout < 0:
			return sandbox.JobsConfig{}, errors.New("--launcher-pending-timeout must not be negative; 0 turns the timeout off")
		case *f.maxSandboxes < 1:
			return sandbox.JobsConfig{}, errors.New("--launcher-max-sandboxes must be at least 1; to start none, turn the launcher off with --launcher=false")
		}
	}
	return sandbox.JobsConfig{
		Namespace:          *f.namespace,
		Image:              *f.image,
		ServiceAccountName: *f.serviceAccount,
		ServerURL:          *f.serverURL,
		RuntimeClassName:   *f.runtimeCl,
		NodeSelector:       nodeSelector,
		Tolerations:        tolerations,
		Resources:          corev1.ResourceRequirements{Requests: requests, Limits: limits},
		APITokenSecret:     *f.apiTokenSecret,
		ModelKeySecret:     *f.modelKeySecret,
	}, nil
}

func splitList(s string) []string {
	out := []string{}
	for item := range strings.SplitSeq(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// newJobs connects to the cluster the server runs in and returns the Job
// runtime the launcher starts sandboxes with.
func newJobs(cfg sandbox.JobsConfig) (*sandbox.Jobs, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("the launcher starts Jobs in the cluster this server runs in, and found no in-cluster config (%w). "+
			"Run factory serve in a Kubernetes pod, or turn the launcher off with --launcher=false", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return sandbox.NewJobs(client, restConfig, cfg)
}

// newLauncher builds the launcher. host, the code host for server-side
// steps, is nil without a GitHub App.
func (f launcherFlags) newLauncher(rt sandbox.Runtime, svc *work.Service, pool *pgxpool.Pool, host func(context.Context, string) (codehost.Host, error), logger *slog.Logger) *launcher.Launcher {
	return &launcher.Launcher{
		Work: svc, Pool: pool, Runtime: rt, MaxSandboxes: *f.maxSandboxes, Host: host,
		Capabilities: splitList(*f.capabilities), AllowedImages: splitList(*f.allowedImages),
		PendingTimeout: *f.pendingTimeout, PollInterval: *f.pollInterval, ChecksGrace: checksGrace, Logger: logger,
	}
}

// githubHost returns the code host for a repository, or nil when mintToken
// is (no GitHub App). It keeps one token per repository, minted on first use
// and again when it nears expiry, so the poller's read of every open pull
// request does not mint a token each time.
func githubHost(mintToken func(context.Context, string) (codehost.Token, error), githubAPI string) func(context.Context, string) (codehost.Host, error) {
	if mintToken == nil {
		return nil
	}
	var mu sync.Mutex
	tokens := map[string]*codehost.RefreshingToken{}
	return func(_ context.Context, repo string) (codehost.Host, error) {
		mu.Lock()
		defer mu.Unlock()
		t, ok := tokens[repo]
		if !ok {
			t = &codehost.RefreshingToken{Fetch: func(ctx context.Context) (codehost.Token, error) { return mintToken(ctx, repo) }}
			tokens[repo] = t
		}
		return codehost.GitHub{API: githubAPI, Repo: repo, Tokens: t.Get}, nil
	}
}
