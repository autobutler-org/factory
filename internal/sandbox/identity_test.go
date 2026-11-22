package sandbox

import (
	"errors"
	"strings"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

// reviewer answers TokenReviews the way the API server does for the tokens
// it knows, by token.
func reviewer(t *testing.T, known map[string]authnv1.TokenReviewStatus) k8stesting.ReactionFunc {
	t.Helper()
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authnv1.TokenReview).DeepCopy()
		if strings.Join(review.Spec.Audiences, ",") != tokenAudience {
			t.Errorf("review audiences = %v, want [%s]", review.Spec.Audiences, tokenAudience)
		}
		status, ok := known[review.Spec.Token]
		if !ok {
			status = authnv1.TokenReviewStatus{Error: "invalid bearer token, square/go-jose: error in cryptographic primitive"}
		}
		review.Status = status
		return true, review, nil
	}
}

func podStatus(name, uid string) authnv1.TokenReviewStatus {
	return authnv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{tokenAudience},
		User: authnv1.UserInfo{
			Username: "system:serviceaccount:factory-workers:factory-worker",
			Extra: map[string]authnv1.ExtraValue{
				extraPodName: {name},
				extraPodUID:  {uid},
			},
		},
	}
}

func jobPod(name, uid, job string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "factory-workers", UID: types.UID(uid),
		Labels: map[string]string{batchv1.JobNameLabel: job},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "Job", Name: job, Controller: new(true),
		}},
	}}
}

func TestJobsIdentify(t *testing.T) {
	t.Parallel()
	j, client := newTestJobs(t)
	if _, err := j.Ensure(t.Context(), "worker-a1", "item-1/o/r", ""); err != nil {
		t.Fatal(err)
	}
	stranger := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "not-ours", Namespace: "factory-workers"}}
	if _, err := client.BatchV1().Jobs("factory-workers").Create(t.Context(), stranger, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []*corev1.Pod{
		jobPod("worker-a1-xyz", "uid-1", "worker-a1"),
		jobPod("not-ours-abc", "uid-3", "not-ours"),
		{ObjectMeta: metav1.ObjectMeta{Name: "loose", Namespace: "factory-workers", UID: "uid-4"}},
	} {
		if _, err := client.CoreV1().Pods("factory-workers").Create(t.Context(), p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	wrongSA := podStatus("worker-a1-xyz", "uid-1")
	wrongSA.User.Username = "system:serviceaccount:factory-workers:default"
	otherNS := podStatus("worker-a1-xyz", "uid-1")
	otherNS.User.Username = "system:serviceaccount:factory:factory-worker"
	noPod := podStatus("", "")
	noPod.User.Extra = nil
	audience := podStatus("worker-a1-xyz", "uid-1")
	audience.Audiences = []string{"https://kubernetes.default.svc"}
	client.PrependReactor("create", "tokenreviews", reviewer(t, map[string]authnv1.TokenReviewStatus{
		"good":      podStatus("worker-a1-xyz", "uid-1"),
		"expired":   {Error: "[invalid bearer token, service account token has expired]"},
		"audience":  {Error: `[invalid bearer token, token audiences ["https://kubernetes.default.svc"] is invalid for the target audiences ["factory"]]`},
		"aud-reply": audience,
		"wrong-sa":  wrongSA,
		"other-ns":  otherNS,
		"no-pod":    noPod,
		"gone-pod":  podStatus("worker-a1-old", "uid-0"),
		"new-uid":   podStatus("worker-a1-xyz", "uid-2"),
		"stranger":  podStatus("not-ours-abc", "uid-3"),
		"loose":     podStatus("loose", "uid-4"),
	}))

	got, err := j.Identify(t.Context(), "good")
	if err != nil {
		t.Fatalf("Identify(good): %v", err)
	}
	if got != (Identity{Sandbox: "worker-a1", Key: "item-1/o/r", Pod: "worker-a1-xyz", PodUID: "uid-1"}) {
		t.Errorf("Identify(good) = %+v, want worker-a1's pod with its sandbox key", got)
	}

	for _, tt := range []struct{ token, want string }{
		{"", "no token"},
		{"forged", "invalid bearer token"},
		{"expired", "has expired"},
		{"audience", "audiences"},
		{"aud-reply", "audience"},
		{"wrong-sa", "factory-workers:factory-worker"},
		{"other-ns", "factory-workers:factory-worker"},
		{"no-pod", "not bound to a pod"},
		{"gone-pod", "not found"},
		{"new-uid", "uid"},
		{"stranger", "not a factory worker"},
		{"loose", "not a factory worker"},
	} {
		t.Run(tt.token, func(t *testing.T) {
			_, err := j.Identify(t.Context(), tt.token)
			if !errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Identify(%s) error = %v, want ErrUnauthenticated naming %q", tt.token, err, tt.want)
			}
		})
	}
}

func TestJobsIdentifyReviewFails(t *testing.T) {
	t.Parallel()
	j, client := newTestJobs(t)
	client.PrependReactor("create", "tokenreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New(`tokenreviews.authentication.k8s.io is forbidden: User "system:serviceaccount:factory:factory" cannot create resource "tokenreviews"`)
	})
	_, err := j.Identify(t.Context(), "good")
	if err == nil || errors.Is(err, ErrUnauthenticated) || !strings.Contains(err.Error(), "system:auth-delegator") {
		t.Errorf("Identify error = %v, want a server-side failure that names the RBAC it needs", err)
	}
}
