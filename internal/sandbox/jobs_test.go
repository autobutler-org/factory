package sandbox

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

var testConfig = JobsConfig{
	Namespace:          "factory-workers",
	Image:              "ghcr.io/autobutler-org/factory-worker@sha256:abc",
	ServiceAccountName: "factory-worker",
	ServerURL:          "http://factory.factory.svc:8080",
	RuntimeClassName:   "gvisor",
}

func newTestJobs(t *testing.T, objects ...any) (*Jobs, *fake.Clientset) {
	t.Helper()
	client := fake.NewClientset()
	for _, o := range objects {
		var err error
		switch o := o.(type) {
		case *batchv1.Job:
			_, err = client.BatchV1().Jobs(o.Namespace).Create(t.Context(), o, metav1.CreateOptions{})
		case *corev1.Pod:
			_, err = client.CoreV1().Pods(o.Namespace).Create(t.Context(), o, metav1.CreateOptions{})
		default:
			t.Fatalf("unexpected object %T", o)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	j, err := NewJobs(client, &rest.Config{Host: "https://kube.example:6443"}, testConfig)
	if err != nil {
		t.Fatalf("NewJobs: %v", err)
	}
	return j, client
}

func TestNewJobsRequiresConfig(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	restConfig := &rest.Config{Host: "https://kube.example:6443"}
	tests := []struct {
		name string
		edit func(*JobsConfig)
		want string
	}{
		{"namespace", func(c *JobsConfig) { c.Namespace = "" }, "namespace"},
		{"image", func(c *JobsConfig) { c.Image = "" }, "image"},
		{"server URL", func(c *JobsConfig) { c.ServerURL = "" }, "server URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig
			tt.edit(&cfg)
			_, err := NewJobs(client, restConfig, cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("NewJobs error = %v, want one naming %q", err, tt.want)
			}
		})
	}
}

func TestJobsEnsureCreatesWorkerJob(t *testing.T) {
	t.Parallel()
	j, client := newTestJobs(t)

	sb, err := j.Ensure(t.Context(), "worker-a1")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if sb != (Sandbox{Name: "worker-a1", Phase: PhasePending}) {
		t.Errorf("Ensure = %+v, want worker-a1 pending", sb)
	}

	job, err := client.BatchV1().Jobs("factory-workers").Get(t.Context(), "worker-a1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Labels[labelManagedBy] != managedByValue {
		t.Errorf("job labels = %v, want %s=%s", job.Labels, labelManagedBy, managedByValue)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0: a failed worker is retried by re-claiming, not by the Job", job.Spec.BackoffLimit)
	}
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Error("ttlSecondsAfterFinished is unset, want finished Jobs cleaned up")
	}
	pod := job.Spec.Template.Spec
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.RestartPolicy)
	}
	if pod.ServiceAccountName != "factory-worker" {
		t.Errorf("serviceAccountName = %q", pod.ServiceAccountName)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken is not false: the worker must not hold a Kubernetes API token")
	}
	if pod.RuntimeClassName == nil || *pod.RuntimeClassName != "gvisor" {
		t.Errorf("runtimeClassName = %v, want gvisor", pod.RuntimeClassName)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Name != workerContainer || c.Image != testConfig.Image {
		t.Errorf("container = %s %s", c.Name, c.Image)
	}
	if strings.Join(c.Args, " ") != "worker" {
		t.Errorf("args = %v, want [worker]", c.Args)
	}
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	for k, want := range map[string]string{
		"FACTORY_URL":        testConfig.ServerURL,
		"FACTORY_SANDBOX":    "worker-a1",
		"FACTORY_TOKEN_FILE": tokenDir + "/" + tokenFile,
	} {
		if env[k] != want {
			t.Errorf("env %s = %q, want %q", k, env[k], want)
		}
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].Projected == nil {
		t.Fatalf("volumes = %+v, want one projected token volume", pod.Volumes)
	}
	tok := pod.Volumes[0].Projected.Sources[0].ServiceAccountToken
	if tok == nil || tok.Audience != tokenAudience || tok.Path != tokenFile {
		t.Errorf("token projection = %+v, want audience %q at %q", tok, tokenAudience, tokenFile)
	}
}

func TestJobsEnsureLeavesExistingJob(t *testing.T) {
	t.Parallel()
	existing := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-a1", Namespace: "factory-workers", Labels: workerLabels()},
		Status:     batchv1.JobStatus{Active: 1, Ready: new(int32(1))},
	}
	j, _ := newTestJobs(t, existing)

	sb, err := j.Ensure(t.Context(), "worker-a1")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if sb.Phase != PhaseRunning {
		t.Errorf("phase = %q, want running from the existing Job", sb.Phase)
	}
}

func TestJobsEnsureRejectsEmptyName(t *testing.T) {
	t.Parallel()
	j, _ := newTestJobs(t)
	if _, err := j.Ensure(t.Context(), ""); err == nil {
		t.Error("Ensure(\"\") succeeded, want an error")
	}
}

func TestJobsStatusPhases(t *testing.T) {
	t.Parallel()
	condition := func(typ batchv1.JobConditionType) []batchv1.JobCondition {
		return []batchv1.JobCondition{{Type: typ, Status: corev1.ConditionTrue}}
	}
	tests := []struct {
		name   string
		status batchv1.JobStatus
		want   Phase
	}{
		{"just created", batchv1.JobStatus{}, PhasePending},
		{"pod scheduled, not ready", batchv1.JobStatus{Active: 1, Ready: new(int32(0))}, PhasePending},
		{"pod ready", batchv1.JobStatus{Active: 1, Ready: new(int32(1))}, PhaseRunning},
		{"complete", batchv1.JobStatus{Conditions: condition(batchv1.JobComplete)}, PhaseSucceeded},
		{"failed", batchv1.JobStatus{Conditions: condition(batchv1.JobFailed)}, PhaseFailed},
		{"failure target before failed", batchv1.JobStatus{Active: 1, Ready: new(int32(1)), Conditions: condition(batchv1.JobFailureTarget)}, PhaseFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "factory-workers", Labels: workerLabels()},
				Status:     tt.status,
			}
			j, _ := newTestJobs(t, job)
			sb, err := j.Status(t.Context(), "w")
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if sb.Phase != tt.want {
				t.Errorf("phase = %q, want %q", sb.Phase, tt.want)
			}
		})
	}
}

func TestJobsStatusNotFound(t *testing.T) {
	t.Parallel()
	j, _ := newTestJobs(t)
	if _, err := j.Status(t.Context(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestJobsListOnlyFactoryJobs(t *testing.T) {
	t.Parallel()
	mine := func(name string) *batchv1.Job {
		return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "factory-workers", Labels: workerLabels()}}
	}
	other := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "factory-workers"}}
	j, _ := newTestJobs(t, mine("w2"), other, mine("w1"))

	list, err := j.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].Name != "w1" || list[1].Name != "w2" {
		t.Errorf("List = %+v, want w1 and w2 only, in name order", list)
	}
}

func TestJobsDestroy(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "factory-workers", Labels: workerLabels()}}
	j, client := newTestJobs(t, job)

	if err := j.Destroy(t.Context(), "w"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, err := client.BatchV1().Jobs("factory-workers").Get(t.Context(), "w", metav1.GetOptions{}); err == nil {
		t.Error("job still exists after Destroy")
	}
	if err := j.Destroy(t.Context(), "w"); err != nil {
		t.Errorf("Destroy of a missing sandbox: %v, want nil", err)
	}
}

type recordingExecutor struct {
	opts remotecommand.StreamOptions
}

func (r *recordingExecutor) Stream(remotecommand.StreamOptions) error {
	return errors.New("not used")
}

func (r *recordingExecutor) StreamWithContext(_ context.Context, opts remotecommand.StreamOptions) error {
	r.opts = opts
	return nil
}

func workerPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-x7k2p",
			Namespace: "factory-workers",
			Labels:    map[string]string{batchv1.JobNameLabel: name},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestJobsExec(t *testing.T) {
	t.Parallel()
	j, _ := newTestJobs(t, workerPod("w", corev1.PodRunning))
	exec := &recordingExecutor{}
	var gotURL *url.URL
	j.newExecutor = func(u *url.URL) (remotecommand.Executor, error) {
		gotURL = u
		return exec, nil
	}
	var out strings.Builder
	in := strings.NewReader("ls\n")

	err := j.Exec(t.Context(), "w", ExecRequest{Command: []string{"sh", "-c", "git status"}, Stdin: in, Stdout: &out, TTY: true})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if gotURL == nil {
		t.Fatal("no executor was built")
	}
	if want := "/api/v1/namespaces/factory-workers/pods/w-x7k2p/exec"; gotURL.Path != want {
		t.Errorf("exec path = %q, want %q", gotURL.Path, want)
	}
	q := gotURL.Query()
	if got := q["command"]; strings.Join(got, "|") != "sh|-c|git status" {
		t.Errorf("command = %v", got)
	}
	if q.Get("container") != workerContainer || q.Get("stdin") != "true" || q.Get("stdout") != "true" || q.Get("tty") != "true" {
		t.Errorf("query = %v, want container=%s stdin stdout tty", q, workerContainer)
	}
	if q.Has("stderr") && q.Get("stderr") == "true" {
		t.Errorf("stderr requested with no Stderr writer: %v", q)
	}
	if exec.opts.Stdin != in || exec.opts.Stdout != &out || !exec.opts.Tty {
		t.Errorf("stream options = %+v, want the request's streams and TTY", exec.opts)
	}
}

func TestJobsExecErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pods []any
		req  ExecRequest
		want error
	}{
		{"no pod", nil, ExecRequest{Command: []string{"true"}}, ErrNotFound},
		{"pod pending", []any{workerPod("w", corev1.PodPending)}, ExecRequest{Command: []string{"true"}}, ErrNotRunning},
		{"pod finished", []any{workerPod("w", corev1.PodSucceeded)}, ExecRequest{Command: []string{"true"}}, ErrNotRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j, _ := newTestJobs(t, tt.pods...)
			j.newExecutor = func(*url.URL) (remotecommand.Executor, error) {
				t.Error("executor built for a sandbox that cannot exec")
				return &recordingExecutor{}, nil
			}
			if err := j.Exec(t.Context(), "w", tt.req); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}

	t.Run("empty command", func(t *testing.T) {
		t.Parallel()
		j, _ := newTestJobs(t, workerPod("w", corev1.PodRunning))
		if err := j.Exec(t.Context(), "w", ExecRequest{}); err == nil {
			t.Error("Exec with no command succeeded, want an error")
		}
	})
}
