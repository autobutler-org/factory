package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
)

const (
	labelName       = "app.kubernetes.io/name"
	labelManagedBy  = "app.kubernetes.io/managed-by"
	nameValue       = "factory-worker"
	managedByValue  = "factory"
	workerContainer = "worker"

	// The worker authenticates to the factory server with a projected
	// ServiceAccount token for this audience (issue #15).
	tokenAudience   = "factory"
	tokenDir        = "/var/run/factory"
	tokenFile       = "token"
	tokenVolume     = "factory-token"
	tokenExpiration = int64(3600)

	// Finished Jobs stay this long so their logs can be read, then
	// Kubernetes deletes them.
	finishedJobTTL = int32(3600)
)

// JobsConfig is what every worker Job is built from.
type JobsConfig struct {
	// Namespace the worker Jobs run in. Required.
	Namespace string
	// Image is the worker image (issue #11). Required.
	Image string
	// ServiceAccountName is the identity the worker's token is issued for.
	// Empty means the namespace's default ServiceAccount.
	ServiceAccountName string
	// ServerURL is the factory server address the worker registers with. Required.
	ServerURL string
	// RuntimeClassName selects a sandboxing runtime such as gVisor. Empty
	// means the cluster default.
	RuntimeClassName string
}

// Jobs is a Runtime that runs each sandbox as a Kubernetes Job with one pod,
// and execs into it through the Kubernetes exec API. It needs RBAC in its
// namespace to create, get, list, and delete Jobs, list Pods, and create
// pods/exec.
type Jobs struct {
	client      kubernetes.Interface
	cfg         JobsConfig
	podREST     rest.Interface
	newExecutor func(*url.URL) (remotecommand.Executor, error)
}

var _ Runtime = (*Jobs)(nil)

// NewJobs returns a Jobs runtime. restConfig is the same config client was
// built from; exec sessions use it to reach the API server.
func NewJobs(client kubernetes.Interface, restConfig *rest.Config, cfg JobsConfig) (*Jobs, error) {
	switch {
	case cfg.Namespace == "":
		return nil, errors.New("sandbox jobs: namespace is required")
	case cfg.Image == "":
		return nil, errors.New("sandbox jobs: worker image is required")
	case cfg.ServerURL == "":
		return nil, errors.New("sandbox jobs: factory server URL is required")
	}
	podConfig := rest.CopyConfig(restConfig)
	podConfig.APIPath = "/api"
	podConfig.GroupVersion = &corev1.SchemeGroupVersion
	podConfig.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	podREST, err := rest.RESTClientFor(podConfig)
	if err != nil {
		return nil, fmt.Errorf("sandbox jobs: build exec client: %w", err)
	}
	return &Jobs{
		client:  client,
		cfg:     cfg,
		podREST: podREST,
		newExecutor: func(u *url.URL) (remotecommand.Executor, error) {
			return newExecutor(restConfig, u)
		},
	}, nil
}

// newExecutor prefers the WebSocket exec protocol and falls back to SPDY on
// API servers that do not upgrade, the same way kubectl exec does.
func newExecutor(cfg *rest.Config, u *url.URL) (remotecommand.Executor, error) {
	ws, err := remotecommand.NewWebSocketExecutor(cfg, http.MethodGet, u.String())
	if err != nil {
		return nil, fmt.Errorf("websocket executor: %w", err)
	}
	spdy, err := remotecommand.NewSPDYExecutor(cfg, http.MethodPost, u)
	if err != nil {
		return nil, fmt.Errorf("spdy executor: %w", err)
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return nil, fmt.Errorf("fallback executor: %w", err)
	}
	return exec, nil
}

func workerLabels() map[string]string {
	return map[string]string{labelName: nameValue, labelManagedBy: managedByValue}
}

func (j *Jobs) job(name string) *batchv1.Job {
	var runtimeClass *string
	if j.cfg.RuntimeClassName != "" {
		runtimeClass = new(j.cfg.RuntimeClassName)
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: j.cfg.Namespace, Labels: workerLabels()},
		Spec: batchv1.JobSpec{
			// A worker that dies is not retried by the Job: its item goes
			// back to ready and the next reconcile starts a fresh sandbox.
			BackoffLimit:            new(int32(0)),
			TTLSecondsAfterFinished: new(finishedJobTTL),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: workerLabels()},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					ServiceAccountName:           j.cfg.ServiceAccountName,
					AutomountServiceAccountToken: new(false),
					RuntimeClassName:             runtimeClass,
					Containers: []corev1.Container{{
						Name:  workerContainer,
						Image: j.cfg.Image,
						Args:  []string{"worker"},
						Env: []corev1.EnvVar{
							{Name: "FACTORY_URL", Value: j.cfg.ServerURL},
							{Name: "FACTORY_SANDBOX", Value: name},
							{Name: "FACTORY_TOKEN_FILE", Value: tokenDir + "/" + tokenFile},
						},
						VolumeMounts: []corev1.VolumeMount{{Name: tokenVolume, MountPath: tokenDir, ReadOnly: true}},
					}},
					Volumes: []corev1.Volume{{
						Name: tokenVolume,
						VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
								Audience:          tokenAudience,
								ExpirationSeconds: new(tokenExpiration),
								Path:              tokenFile,
							}}},
						}},
					}},
				},
			},
		},
	}
}

// phase maps a Job's status to a sandbox phase.
func phase(job *batchv1.Job) Phase {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete, batchv1.JobSuccessCriteriaMet:
			return PhaseSucceeded
		case batchv1.JobFailed, batchv1.JobFailureTarget:
			return PhaseFailed
		}
	}
	if job.Status.Active > 0 && job.Status.Ready != nil && *job.Status.Ready > 0 {
		return PhaseRunning
	}
	return PhasePending
}

// Ensure implements Runtime.
func (j *Jobs) Ensure(ctx context.Context, name string) (Sandbox, error) {
	if name == "" {
		return Sandbox{}, errors.New("ensure sandbox: name is empty")
	}
	created, err := j.client.BatchV1().Jobs(j.cfg.Namespace).Create(ctx, j.job(name), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return j.Status(ctx, name)
	}
	if err != nil {
		return Sandbox{}, fmt.Errorf("ensure sandbox %q: create job: %w", name, err)
	}
	return Sandbox{Name: name, Phase: phase(created)}, nil
}

// Status implements Runtime.
func (j *Jobs) Status(ctx context.Context, name string) (Sandbox, error) {
	job, err := j.client.BatchV1().Jobs(j.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Sandbox{}, fmt.Errorf("sandbox %q: %w", name, ErrNotFound)
	}
	if err != nil {
		return Sandbox{}, fmt.Errorf("sandbox %q: get job: %w", name, err)
	}
	return Sandbox{Name: name, Phase: phase(job)}, nil
}

// List implements Runtime.
func (j *Jobs) List(ctx context.Context) ([]Sandbox, error) {
	jobs, err := j.client.BatchV1().Jobs(j.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(workerLabels()).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	out := make([]Sandbox, 0, len(jobs.Items))
	for i := range jobs.Items {
		out = append(out, Sandbox{Name: jobs.Items[i].Name, Phase: phase(&jobs.Items[i])})
	}
	slices.SortFunc(out, func(a, b Sandbox) int { return cmp.Compare(a.Name, b.Name) })
	return out, nil
}

// Destroy implements Runtime. Deleting the Job deletes its pod, which sends
// the worker SIGTERM and gives it the pod's grace period to release its item.
func (j *Jobs) Destroy(ctx context.Context, name string) error {
	err := j.client.BatchV1().Jobs(j.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: new(metav1.DeletePropagationBackground),
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("destroy sandbox %q: %w", name, err)
	}
	return nil
}

// Exec implements Runtime.
func (j *Jobs) Exec(ctx context.Context, name string, req ExecRequest) error {
	if len(req.Command) == 0 {
		return errors.New("exec: command is empty")
	}
	pod, err := j.runningPod(ctx, name)
	if err != nil {
		return err
	}
	u := j.podREST.Post().
		Namespace(j.cfg.Namespace).
		Resource("pods").
		Name(pod).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: workerContainer,
			Command:   req.Command,
			Stdin:     req.Stdin != nil,
			Stdout:    req.Stdout != nil,
			Stderr:    req.Stderr != nil && !req.TTY,
			TTY:       req.TTY,
		}, scheme.ParameterCodec).
		URL()
	exec, err := j.newExecutor(u)
	if err != nil {
		return fmt.Errorf("exec in sandbox %q: %w", name, err)
	}
	opts := remotecommand.StreamOptions{Stdin: req.Stdin, Stdout: req.Stdout, Tty: req.TTY}
	if !req.TTY {
		// With a TTY the terminal merges stderr into stdout.
		opts.Stderr = req.Stderr
	}
	if err := exec.StreamWithContext(ctx, opts); err != nil {
		return fmt.Errorf("exec in sandbox %q: %w", name, err)
	}
	return nil
}

// runningPod returns the name of the named sandbox's running pod.
func (j *Jobs) runningPod(ctx context.Context, name string) (string, error) {
	pods, err := j.client.CoreV1().Pods(j.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{batchv1.JobNameLabel: name}).String(),
	})
	if err != nil {
		return "", fmt.Errorf("sandbox %q: list pods: %w", name, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("sandbox %q has no pod: %w", name, ErrNotFound)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("sandbox %q pod is %s: %w", name, pods.Items[0].Status.Phase, ErrNotRunning)
}
