package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	authnv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The TokenReview extra keys that name the pod a projected ServiceAccount
// token is bound to.
const (
	extraPodName = "authentication.kubernetes.io/pod-name"
	extraPodUID  = "authentication.kubernetes.io/pod-uid"
)

// ErrUnauthenticated is a worker token that does not prove the caller is a
// pod of a sandbox this runtime started.
var ErrUnauthenticated = errors.New("the worker token was refused")

// Identity is the sandbox a worker pod belongs to, as its projected
// ServiceAccount token proves it.
type Identity struct {
	// Sandbox is the Job's name, the name its worker registers under.
	Sandbox string
	// Key is the sandbox key the launcher gave the Job.
	Key string
	// Pod and PodUID are the pod the token is bound to.
	Pod, PodUID string
}

// Identify verifies a worker pod's projected ServiceAccount token with a
// TokenReview for the audience "factory", and returns the sandbox of the pod
// the token is bound to. The API server checks the signature, the expiry,
// the audience and that the pod still exists; Identify then checks that the
// token is the worker ServiceAccount's in this namespace and that its pod is
// controlled by a Job this runtime created, whose sandbox key it reads from
// the Job, not from the caller. A token that fails any check is
// ErrUnauthenticated; a review the API server cannot answer is another error.
func (j *Jobs) Identify(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, fmt.Errorf("%w: no token", ErrUnauthenticated)
	}
	review, err := j.client.AuthenticationV1().TokenReviews().Create(ctx, &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{tokenAudience}},
	}, metav1.CreateOptions{})
	if err != nil {
		return Identity{}, fmt.Errorf("review the worker token: %w; the server's ServiceAccount needs the ClusterRole system:auth-delegator", err)
	}
	st := review.Status
	if !st.Authenticated {
		reason := st.Error
		if reason == "" {
			reason = "the API server did not authenticate it"
		}
		return Identity{}, fmt.Errorf("%w: %s", ErrUnauthenticated, reason)
	}
	if !slices.Contains(st.Audiences, tokenAudience) {
		return Identity{}, fmt.Errorf("%w: the token is not for the audience %q", ErrUnauthenticated, tokenAudience)
	}
	want := "system:serviceaccount:" + j.cfg.Namespace + ":" + cmp.Or(j.cfg.ServiceAccountName, "default")
	if st.User.Username != want {
		return Identity{}, fmt.Errorf("%w: the token is %s's, not %s's", ErrUnauthenticated, st.User.Username, want)
	}
	podName, podUID := first(st.User.Extra[extraPodName]), first(st.User.Extra[extraPodUID])
	if podName == "" || podUID == "" {
		return Identity{}, fmt.Errorf("%w: the token is not bound to a pod; send the projected token the Job mounts, not a Secret-based one", ErrUnauthenticated)
	}
	pod, err := j.client.CoreV1().Pods(j.cfg.Namespace).Get(ctx, podName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Identity{}, fmt.Errorf("%w: pod %s not found", ErrUnauthenticated, podName)
	case err != nil:
		return Identity{}, fmt.Errorf("get the worker's pod %s: %w", podName, err)
	case string(pod.UID) != podUID:
		return Identity{}, fmt.Errorf("%w: pod %s has uid %s, the token names uid %s", ErrUnauthenticated, podName, pod.UID, podUID)
	}
	owner := metav1.GetControllerOf(pod)
	jobName := pod.Labels[batchv1.JobNameLabel]
	if owner == nil || owner.Kind != "Job" || owner.Name != jobName {
		return Identity{}, fmt.Errorf("%w: pod %s is not a factory worker's: no Job controls it", ErrUnauthenticated, podName)
	}
	job, err := j.client.BatchV1().Jobs(j.cfg.Namespace).Get(ctx, jobName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return Identity{}, fmt.Errorf("%w: Job %s not found", ErrUnauthenticated, jobName)
	case err != nil:
		return Identity{}, fmt.Errorf("get the worker's Job %s: %w", jobName, err)
	}
	key := job.Annotations[annotationKey]
	if job.Labels[labelManagedBy] != managedByValue || job.Labels[labelName] != nameValue || key == "" {
		return Identity{}, fmt.Errorf("%w: Job %s is not a factory worker's: the launcher did not create it", ErrUnauthenticated, jobName)
	}
	return Identity{Sandbox: job.Name, Key: key, Pod: pod.Name, PodUID: podUID}, nil
}

func first(v authnv1.ExtraValue) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
