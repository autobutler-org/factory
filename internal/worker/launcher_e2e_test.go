//go:build e2e

package worker_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/codehost/fakegh"
	"github.com/autobutler-org/factory/internal/harness"
	"github.com/autobutler-org/factory/internal/launcher"
	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/autobutler-org/factory/internal/work"
	"github.com/autobutler-org/factory/internal/worker"
	authnv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// cluster stands in for Kubernetes under the real sandbox.Jobs runtime: a
// Job the launcher creates gets a pod and a projected token the cluster's
// TokenReview vouches for, runs the real worker loop in-process, configured
// from the Job's container environment as factory worker would read it, and
// is marked complete when the worker exits.
type cluster struct {
	e      *env
	client *fake.Clientset
	wg     sync.WaitGroup
	mu     sync.Mutex
	jobs   []*batchv1.Job
	errs   []error
	// tokens are the pods' projected tokens, by token.
	tokens map[string]*corev1.Pod
}

// review answers a TokenReview for a pod token this cluster issued.
func (c *cluster) review(action k8stesting.Action) (bool, runtime.Object, error) {
	review := action.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview).DeepCopy()
	c.mu.Lock()
	pod, ok := c.tokens[review.Spec.Token]
	c.mu.Unlock()
	if !ok || !slices.Contains(review.Spec.Audiences, "factory") {
		review.Status.Error = "invalid bearer token"
		return true, review, nil
	}
	review.Status = authnv1.TokenReviewStatus{
		Authenticated: true, Audiences: []string{"factory"},
		User: authnv1.UserInfo{
			Username: "system:serviceaccount:" + pod.Namespace + ":factory-worker",
			Extra: map[string]authnv1.ExtraValue{
				"authentication.kubernetes.io/pod-name": {pod.Name},
				"authentication.kubernetes.io/pod-uid":  {string(pod.UID)},
			},
		},
	}
	return true, review, nil
}

func (c *cluster) create(action k8stesting.Action) (bool, runtime.Object, error) {
	job := action.(k8stesting.CreateAction).GetObject().(*batchv1.Job).DeepCopy()
	c.mu.Lock()
	c.jobs = append(c.jobs, job)
	c.mu.Unlock()
	env := map[string]string{}
	for _, v := range job.Spec.Template.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	c.wg.Go(func() {
		// The Job controller's pod, and the kubelet's token for it at
		// FACTORY_TOKEN_FILE.
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name: job.Name + "-pod", Namespace: job.Namespace, UID: types.UID("uid-" + job.Name),
			Labels:          map[string]string{batchv1.JobNameLabel: job.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, Controller: new(true)}},
		}}
		if _, err := c.client.CoreV1().Pods(job.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
			c.e.t.Error(err)
			return
		}
		token := "projected-token-of-" + pod.Name
		c.mu.Lock()
		c.tokens[token] = pod
		c.mu.Unlock()
		if env["FACTORY_TOKEN_FILE"] == "" {
			c.e.t.Errorf("Job %s has no FACTORY_TOKEN_FILE", job.Name)
		}
		err := worker.Run(c.e.t.Context(), worker.Config{
			Client:     &worker.Client{URL: env["FACTORY_URL"], Token: func() (string, error) { return token, nil }},
			Name:       env["FACTORY_SANDBOX"],
			SandboxKey: env["FACTORY_SANDBOX_KEY"],
			// FACTORY_WORK_DIR is /workspace, the pod's emptyDir; here a
			// directory of the test's own.
			WorkDir:   c.e.t.TempDir(),
			Harnesses: []harness.Harness{&harness.ClaudeCode{Bin: filepath.Join(c.e.agentDir, "agent")}},
			GitURL:    "file://" + c.e.origin,
			Host: func(repo string, tokens func(context.Context) (string, error)) codehost.Host {
				return codehost.GitHub{API: c.e.gh.URL, Repo: repo, Tokens: tokens}
			},
			Output: io.Discard,
			Log:    slog.New(slog.NewTextHandler(c.e.t.Output(), nil)),
		})
		condition := batchv1.JobComplete
		if err != nil {
			condition = batchv1.JobFailed
			c.mu.Lock()
			c.errs = append(c.errs, err)
			c.mu.Unlock()
		}
		// The create this reactor saw is stored once the reactor returns.
		for {
			stored, err := c.client.BatchV1().Jobs(job.Namespace).Get(context.Background(), job.Name, metav1.GetOptions{})
			if err == nil {
				stored.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}}
				if _, err := c.client.BatchV1().Jobs(job.Namespace).UpdateStatus(context.Background(), stored, metav1.UpdateOptions{}); err == nil {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return false, nil, nil
}

// TestE2ELauncherStartsTheWorker runs one item from enqueued to completed
// with nothing but launcher passes: the launcher reads the repository's
// .factory/config.toml through fakegh and plans it with one review round,
// starts its chain's sandbox as a Job in the image the config names, and the
// real worker in that sandbox claims only that chain's steps until the chain
// waits on checks. Then the launcher's poll sees the checks pass on fakegh
// and completes the wait, the launcher merges, and the item completes.
func TestE2ELauncherStartsTheWorker(t *testing.T) {
	e := newEnv(t, "test -f hello.txt && touch MARKER", 3)
	const quarkImage = "ghcr.io/autobutler-org/quark-worker:1.4.0"
	allowed := []string{"ghcr.io/autobutler-org/"}
	e.gh.SetFile(".factory/config.toml", e.config+"review_rounds = 1\nimage = \""+quarkImage+"\"\n")
	svc := e.svc.WithConfigs(work.Configs{
		Host: func(_ context.Context, repo string) (codehost.Host, error) {
			return codehost.GitHub{API: e.gh.URL, Repo: repo, Token: ghToken}, nil
		},
		AllowedImages: allowed,
	})
	c := &cluster{e: e, client: fake.NewClientset(), tokens: map[string]*corev1.Pod{}}
	c.client.PrependReactor("create", "jobs", c.create)
	c.client.PrependReactor("create", "tokenreviews", c.review)
	// The server as factory serve --launcher runs it: workers authenticate
	// as their pods, and the people's token cannot act as one.
	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	jobs, err := sandbox.NewJobs(c.client, &rest.Config{Host: "https://kube.test"}, sandbox.JobsConfig{
		Namespace: "factory-workers", Image: "ghcr.io/autobutler-org/factory-worker:test", ServerURL: srv.URL,
		ServiceAccountName: "factory-worker", ModelKeySecret: "factory-model-api-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	handler = api.New(api.Config{
		Work: e.svc, Ready: func(context.Context) error { return nil }, Token: "people-token", Logger: slog.New(slog.DiscardHandler),
		MintToken: func(context.Context, string) (codehost.Token, error) {
			return codehost.Token{Token: ghToken, ExpiresAt: time.Now().Add(time.Hour)}, nil
		},
		IdentifyWorker: jobs.Identify, Sandboxes: jobs,
	})
	l := &launcher.Launcher{
		Work: svc, Pool: e.pool, Runtime: jobs, MaxSandboxes: 2, Capabilities: []string{"claude-code"}, AllowedImages: allowed,
		PendingTimeout: 10 * time.Minute, Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
	}

	deadline := time.Now().Add(3 * time.Minute)
	for e.state().State != "in_review" {
		if time.Now().After(deadline) {
			t.Fatalf("item still %s", e.state().State)
		}
		if _, err := l.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	// The worker exits once its chain waits on checks; a later pass starts
	// nothing for a chain that has nothing runnable.
	c.wg.Wait()
	r, err := l.Pass(t.Context())
	if err != nil || len(r.Started) != 0 {
		t.Fatalf("pass with the chain waiting = %+v, %v; want nothing started", r, err)
	}

	e.inReview()
	if len(c.errs) != 0 {
		t.Errorf("workers failed: %v", c.errs)
	}
	if len(c.jobs) != 1 {
		t.Fatalf("Jobs created = %d, want one sandbox for the one chain", len(c.jobs))
	}
	job := c.jobs[0]
	if image := job.Spec.Template.Spec.Containers[0].Image; image != quarkImage {
		t.Errorf("Job image = %s, want the repository's %s", image, quarkImage)
	}
	g, err := e.svc.Graph(t.Context(), e.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range g.Steps {
		if strings.HasPrefix(s.Key, repo+"/review-") && s.Key != repo+"/review-1" || strings.HasPrefix(s.Key, repo+"/fix-") && !strings.Contains(s.Key, "checks") {
			t.Errorf("review_rounds = 1, but the graph has %s", s.Key)
		}
	}
	envs := map[string]string{}
	for _, v := range job.Spec.Template.Spec.Containers[0].Env {
		envs[v.Name] = v.Value
	}
	key := e.item.ID + "/" + repo
	if envs["FACTORY_SANDBOX"] != job.Name || envs["FACTORY_SANDBOX_KEY"] != key || envs["FACTORY_URL"] != srv.URL || !strings.HasPrefix(job.Name, "worker-") {
		t.Errorf("Job %s env = %v", job.Name, envs)
	}
	if _, ok := envs["FACTORY_API_TOKEN"]; ok {
		t.Errorf("Job %s carries FACTORY_API_TOKEN; a worker pod must hold no shared token", job.Name)
	}
	workers, err := e.svc.Workers(t.Context())
	if err != nil || len(workers) != 1 || workers[0].Name != job.Name {
		t.Errorf("workers = %+v, %v; want the one registered under the Job's name", workers, err)
	}

	// With the GitHub App, the launcher polls: green checks complete
	// await_checks-1, the merge runs in the server, and the item completes.
	e.gh.SetChecks([]fakegh.CheckRun{{ID: 1, Name: "ci", Status: "completed", Conclusion: "success"}})
	l.Host = func(_ context.Context, repo string) (codehost.Host, error) {
		return codehost.GitHub{API: e.gh.URL, Repo: repo, Token: ghToken}, nil
	}
	for e.state().State != "completed" {
		if time.Now().After(deadline) {
			t.Fatalf("item still %s", e.state().State)
		}
		if _, err := l.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s := e.step(repo + "/await_checks-1"); s.State != "succeeded" || !strings.Contains(string(s.Result), `"verdict": "pass"`) ||
		!strings.Contains(string(s.Result), fakegh.HeadSHA) {
		t.Errorf("await_checks-1 = %s %s, want a pass on fakegh's head", s.State, s.Result)
	}
	if n, _ := e.gh.Merges(); n != 1 {
		t.Errorf("merges = %d, want the launcher's one", n)
	}
	if pulls, _ := e.gh.Snapshot(); !pulls[0].Merged {
		t.Errorf("pull = %+v, want merged", pulls[0])
	}
}
