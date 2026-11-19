package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"strings"
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

const launcherUsage = `
Launcher (starts worker sandboxes as Kubernetes Jobs; needs in-cluster config):
  --launcher           run the launcher and the server-side steps (default false)
  --launcher-namespace NS
                       namespace the worker Jobs run in (default factory-workers)
  --launcher-image IMAGE
                       worker image; required with --launcher
  --launcher-server-url URL
                       where workers reach this server; required with --launcher
  --launcher-allowed-images LIST
                       comma-separated image repository prefixes, such as
                       ghcr.io/autobutler-org/, a repository's image (image in
                       .factory/config.toml) may come from; a prefix without a
                       trailing / is one repository. Empty lets no repository
                       choose its image (default empty)
  --launcher-max-sandboxes N
                       most worker sandboxes alive at once (default 4)
  --launcher-pending-timeout DURATION
                       destroy a sandbox still pending after this long (an image
                       that will not pull, a pod that cannot be scheduled) and
                       start a new one; 0 never does (default 10m)
  --launcher-capabilities LIST
                       comma-separated capabilities the worker image registers;
                       items requiring others are left alone (default claude-code)
  --launcher-service-account NAME
                       ServiceAccount of worker pods (default factory-worker)
  --launcher-node-selector LIST
                       key=value,... for worker pods
  --launcher-tolerations LIST
                       key=value:Effect or key:Effect,... for worker pods
  --launcher-requests LIST
  --launcher-limits LIST
                       cpu=..,memory=..,ephemeral-storage=.. of the worker
                       container (defaults cpu=1,memory=2Gi,ephemeral-storage=10Gi
                       and cpu=4,memory=8Gi,ephemeral-storage=20Gi)
  --launcher-api-token-secret NAME
                       Secret in the worker namespace whose key "token" is given
                       to workers as FACTORY_API_TOKEN (default factory-worker-api-token)
  --launcher-model-key-secret NAME
                       Secret in the worker namespace whose key "api-key" is given
                       to workers as ANTHROPIC_API_KEY (default factory-model-api-key)
  --launcher-runtime-class NAME
                       RuntimeClass of worker pods, such as gvisor (default none)
`

// launcherFlags are factory serve's launcher settings, as parsed.
type launcherFlags struct {
	enabled                                   *bool
	namespace, image, serverURL               *string
	allowedImages                             *string
	maxSandboxes                              *int
	pendingTimeout                            *time.Duration
	capabilities, serviceAccount              *string
	nodeSelector, tolerations                 *string
	requests, limits                          *string
	apiTokenSecret, modelKeySecret, runtimeCl *string
}

func addLauncherFlags(fs *flag.FlagSet) launcherFlags {
	return launcherFlags{
		enabled:        fs.Bool("launcher", false, ""),
		namespace:      fs.String("launcher-namespace", "factory-workers", ""),
		image:          fs.String("launcher-image", "", ""),
		serverURL:      fs.String("launcher-server-url", "", ""),
		allowedImages:  fs.String("launcher-allowed-images", "", ""),
		maxSandboxes:   fs.Int("launcher-max-sandboxes", 4, ""),
		pendingTimeout: fs.Duration("launcher-pending-timeout", 10*time.Minute, ""),
		capabilities:   fs.String("launcher-capabilities", "claude-code", ""),
		serviceAccount: fs.String("launcher-service-account", "factory-worker", ""),
		nodeSelector:   fs.String("launcher-node-selector", "", ""),
		tolerations:    fs.String("launcher-tolerations", "", ""),
		requests:       fs.String("launcher-requests", "cpu=1,memory=2Gi,ephemeral-storage=10Gi", ""),
		limits:         fs.String("launcher-limits", "cpu=4,memory=8Gi,ephemeral-storage=20Gi", ""),
		apiTokenSecret: fs.String("launcher-api-token-secret", "factory-worker-api-token", ""),
		modelKeySecret: fs.String("launcher-model-key-secret", "factory-model-api-key", ""),
		runtimeCl:      fs.String("launcher-runtime-class", "", ""),
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
		PendingTimeout: *f.pendingTimeout, Logger: logger,
	}
}

// githubHost returns the code host for a repository, with a token minted for
// it, or nil when mintToken is (no GitHub App).
func githubHost(mintToken func(context.Context, string) (codehost.Token, error), githubAPI string) func(context.Context, string) (codehost.Host, error) {
	if mintToken == nil {
		return nil
	}
	return func(ctx context.Context, repo string) (codehost.Host, error) {
		token, err := mintToken(ctx, repo)
		if err != nil {
			return nil, err
		}
		return codehost.GitHub{API: githubAPI, Repo: repo, Token: token.Token}, nil
	}
}
