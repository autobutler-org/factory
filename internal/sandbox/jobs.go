package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

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
	// annotationKey holds the sandbox key of the chain a Job serves.
	annotationKey = "factory.autobutler.org/sandbox-key"

	// WorkspaceDir is where the worker's emptyDir workspace is mounted.
	WorkspaceDir    = "/workspace"
	workspaceVolume = "workspace"

	// The key in the Secret the model API key comes from.
	modelKeySecretKey = "api-key"

	// The worker registers with the projected ServiceAccount token for this
	// audience (FACTORY_TOKEN_FILE), which the server verifies with a
	// TokenReview (Identify). The pod gets no other factory credential.
	tokenAudience = "factory"
	// TokenPath is where the worker's projected token is mounted.
	TokenPath       = tokenDir + "/" + tokenFile
	tokenDir        = "/var/run/factory"
	tokenFile       = "token"
	tokenVolume     = "factory-token"
	tokenExpiration = int64(3600)

	// Finished Jobs stay this long so their logs can be read, then
	// Kubernetes deletes them.
	finishedJobTTL = int32(3600)

	terminationGrace = int64(60)
)

// JobsConfig is what every worker Job is built from.
type JobsConfig struct {
	// Namespace the worker Jobs run in. Required.
	Namespace string
	// Image is the default worker image (issue #11), for a sandbox ensured
	// without one. Required.
	Image string
	// ServiceAccountName is the identity the worker's token is issued for.
	// Empty means the namespace's default ServiceAccount.
	ServiceAccountName string
	// ServerURL is the factory server address the worker registers with. Required.
	ServerURL string
	// RuntimeClassName selects a sandboxing runtime such as gVisor. Empty
	// means the cluster default.
	RuntimeClassName string
	// NodeSelector and Tolerations place worker pods on the worker nodes.
	NodeSelector map[string]string
	Tolerations  []corev1.Toleration
	// Resources are the worker container's requests and limits, including
	// ephemeral-storage for clones and build output.
	Resources corev1.ResourceRequirements
	// ModelKeySecret names a Secret in Namespace whose key "api-key" is the
	// model API key, given to the worker as ANTHROPIC_API_KEY. Empty gives
	// the worker none.
	ModelKeySecret string
}

// Jobs is a Runtime that runs each sandbox as a Kubernetes Job with one pod,
// and execs into it through the Kubernetes exec API. It needs RBAC in its
// namespace to create, get, list, and delete Jobs, get and list Pods, create
// pods/exec, and get pods/log, and, for Identify, create tokenreviews
// (the ClusterRole system:auth-delegator).
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

func secretEnv(name, secret, key string) []corev1.EnvVar {
	if secret == "" {
		return nil
	}
	return []corev1.EnvVar{{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key,
	}}}}
}

func (j *Jobs) job(name, key, image string) *batchv1.Job {
	if image == "" {
		image = j.cfg.Image
	}
	var runtimeClass *string
	if j.cfg.RuntimeClassName != "" {
		runtimeClass = new(j.cfg.RuntimeClassName)
	}
	env := []corev1.EnvVar{
		{Name: "FACTORY_URL", Value: j.cfg.ServerURL},
		// The name the worker registers under, and the chain it claims.
		{Name: "FACTORY_SANDBOX", Value: name},
		{Name: "FACTORY_SANDBOX_KEY", Value: key},
		{Name: "FACTORY_WORK_DIR", Value: WorkspaceDir},
		// The worker image installs Claude Code only.
		{Name: "FACTORY_HARNESSES", Value: "claude-code"},
		// The pod's own identity, sent once to register.
		{Name: "FACTORY_TOKEN_FILE", Value: TokenPath},
	}
	env = append(env, secretEnv("ANTHROPIC_API_KEY", j.cfg.ModelKeySecret, modelKeySecretKey)...)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: j.cfg.Namespace, Labels: workerLabels(),
			Annotations: map[string]string{annotationKey: key},
		},
		Spec: batchv1.JobSpec{
			// A worker that dies is not retried by the Job: its step goes
			// back to ready and the next pass of the launcher starts a fresh
			// sandbox for the chain.
			BackoffLimit:            new(int32(0)),
			TTLSecondsAfterFinished: new(finishedJobTTL),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: workerLabels(), Annotations: map[string]string{annotationKey: key}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// Time for the worker to release its step on SIGTERM.
					TerminationGracePeriodSeconds: new(terminationGrace),
					ServiceAccountName:            j.cfg.ServiceAccountName,
					AutomountServiceAccountToken:  new(false),
					RuntimeClassName:              runtimeClass,
					NodeSelector:                  j.cfg.NodeSelector,
					Tolerations:                   j.cfg.Tolerations,
					// What Pod Security "restricted" requires. The image runs
					// as a numeric non-root user.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   new(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:      workerContainer,
						Image:     image,
						Args:      []string{"worker"},
						Env:       env,
						Resources: j.cfg.Resources,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: new(false),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: tokenVolume, MountPath: tokenDir, ReadOnly: true},
							{Name: workspaceVolume, MountPath: WorkspaceDir},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: tokenVolume,
							VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
								Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
									Audience:          tokenAudience,
									ExpirationSeconds: new(tokenExpiration),
									Path:              tokenFile,
								}}},
							}},
						},
						{Name: workspaceVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

func sandboxOf(job *batchv1.Job) Sandbox {
	var image string
	if cs := job.Spec.Template.Spec.Containers; len(cs) > 0 {
		image = cs[0].Image
	}
	return Sandbox{Name: job.Name, Key: job.Annotations[annotationKey], Image: image, Phase: phase(job), Created: job.CreationTimestamp.Time}
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
func (j *Jobs) Ensure(ctx context.Context, name, key, image string) (Sandbox, error) {
	if name == "" {
		return Sandbox{}, errors.New("ensure sandbox: name is empty")
	}
	created, err := j.client.BatchV1().Jobs(j.cfg.Namespace).Create(ctx, j.job(name, key, image), metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return j.Status(ctx, name)
	}
	if err != nil {
		return Sandbox{}, fmt.Errorf("ensure sandbox %q: create job: %w", name, err)
	}
	return sandboxOf(created), nil
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
	return sandboxOf(job), nil
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
		out = append(out, sandboxOf(&jobs.Items[i]))
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

// Logs implements Runtime. It reads the worker container's log through the
// pod log API with timestamps, which the cursor is built from: a read after
// a cursor starts at the cursor's whole second (sinceTime has one-second
// granularity) and drops what the cursor has already covered.
func (j *Jobs) Logs(ctx context.Context, name string, q LogQuery) (Logs, error) {
	pod, err := j.startedPod(ctx, name)
	if err != nil {
		return Logs{}, err
	}
	opts := &corev1.PodLogOptions{Container: workerContainer, Timestamps: true}
	var after Cursor
	switch {
	case q.After == nil:
		opts.TailLines = new(int64(q.Tail))
	case !q.After.Time.IsZero():
		after = *q.After
		opts.SinceTime = &metav1.Time{Time: after.Time}
	}
	// The pod log API is read through podREST, not the clientset, so tests
	// can serve it.
	stream, err := j.podREST.Get().
		Namespace(j.cfg.Namespace).
		Resource("pods").
		Name(pod).
		SubResource("log").
		VersionedParams(opts, scheme.ParameterCodec).
		Stream(ctx)
	switch {
	case apierrors.IsNotFound(err):
		return Logs{}, fmt.Errorf("sandbox %q pod %s is gone: %w", name, pod, ErrNotFound)
	case apierrors.IsBadRequest(err) && strings.Contains(err.Error(), "waiting to start"):
		// The container stopped being ready between the pod read and this one.
		_, reason, _ := strings.Cut(err.Error(), "waiting to start: ")
		return Logs{}, &NotStartedError{Reason: reason, Message: err.Error()}
	case err != nil:
		return Logs{}, fmt.Errorf("sandbox %q: read pod log: %w", name, err)
	}
	defer stream.Close()
	logs, err := readLog(stream, after)
	if err != nil {
		return logs, fmt.Errorf("sandbox %q: %w", name, err)
	}
	return logs, nil
}

// startedPod returns the name of the named sandbox's newest pod once its
// worker container has started.
func (j *Jobs) startedPod(ctx context.Context, name string) (string, error) {
	pods, err := j.client.CoreV1().Pods(j.cfg.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{batchv1.JobNameLabel: name}).String(),
	})
	if err != nil {
		return "", fmt.Errorf("sandbox %q: list pods: %w", name, err)
	}
	if len(pods.Items) == 0 {
		if _, err := j.Status(ctx, name); err != nil {
			return "", err
		}
		return "", &NotStartedError{Reason: "NoPod", Message: "the Job has not created its pod yet"}
	}
	pod := slices.MaxFunc(pods.Items, func(a, b corev1.Pod) int {
		return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
	})
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name != workerContainer {
			continue
		}
		if c.State.Running != nil || c.State.Terminated != nil {
			return pod.Name, nil
		}
		if c.State.Waiting != nil {
			return "", &NotStartedError{Reason: c.State.Waiting.Reason, Message: c.State.Waiting.Message}
		}
	}
	return "", &NotStartedError{Reason: string(pod.Status.Phase), Message: pod.Status.Message}
}
