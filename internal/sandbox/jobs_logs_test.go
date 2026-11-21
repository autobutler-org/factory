package sandbox

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// The fake clientset's GetLogs always answers "fake logs", so the log is
// read from an httptest API server instead, which also sees the query.
func newLogJobs(t *testing.T, handler http.HandlerFunc, objects ...any) *Jobs {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	_, client := newTestJobs(t, objects...)
	jobs, err := NewJobs(client, &rest.Config{Host: srv.URL}, testConfig)
	if err != nil {
		t.Fatal(err)
	}
	return jobs
}

func podWithWorker(name string, phase corev1.PodPhase, state corev1.ContainerState) *corev1.Pod {
	p := workerPod(name, phase)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: workerContainer, State: state}}
	return p
}

var running = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}

func TestJobsLogs(t *testing.T) {
	t.Parallel()
	body := stamped(LogLine{Time: t0, Text: `{"msg":"claimed"}`}, LogLine{Time: t0.Add(1500 * time.Millisecond), Text: "two"})
	var mu sync.Mutex
	var queries []url.Values
	j := newLogJobs(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/factory-workers/pods/w-x7k2p/log" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.Query())
		mu.Unlock()
		_, _ = w.Write([]byte(body))
	}, podWithWorker("w", corev1.PodRunning, running))

	tail, err := j.Logs(t.Context(), "w", LogQuery{Tail: 500})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if texts(tail.Lines) != `{"msg":"claimed"},two` || !tail.Lines[0].Time.Equal(t0) {
		t.Errorf("lines = %+v", tail.Lines)
	}
	after := Cursor{Time: t0.Add(1500 * time.Millisecond), Skip: 1}
	if tail.Next != after {
		t.Errorf("next = %+v, want %+v", tail.Next, after)
	}
	next, err := j.Logs(t.Context(), "w", LogQuery{After: &after})
	if err != nil || len(next.Lines) != 0 || next.Next != after {
		t.Errorf("after the last line = %+v, %v; want nothing new and the same cursor", next, err)
	}

	q := queries[0]
	if q.Get("container") != workerContainer || q.Get("timestamps") != "true" || q.Get("tailLines") != "500" || q.Has("sinceTime") {
		t.Errorf("tail query = %v, want the worker container, timestamps and tailLines=500", q)
	}
	q = queries[1]
	if q.Get("sinceTime") != "2026-11-20T10:00:01Z" || q.Has("tailLines") {
		t.Errorf("follow query = %v, want sinceTime at the cursor's second and no tailLines", q)
	}
}

func TestJobsLogsFromTheStart(t *testing.T) {
	t.Parallel()
	var got url.Values
	j := newLogJobs(t, func(w http.ResponseWriter, r *http.Request) { got = r.URL.Query() },
		podWithWorker("w", corev1.PodSucceeded, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}))
	res, err := j.Logs(t.Context(), "w", LogQuery{After: &Cursor{}})
	if err != nil || len(res.Lines) != 0 || res.Next != (Cursor{}) {
		t.Fatalf("Logs = %+v, %v", res, err)
	}
	if got.Has("sinceTime") || got.Has("tailLines") {
		t.Errorf("query = %v, want the whole log from the start", got)
	}
}

func TestJobsLogsErrors(t *testing.T) {
	t.Parallel()
	never := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "the log was read for a sandbox without one", http.StatusTeapot)
	}
	waiting := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull failed"}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "w", Namespace: "factory-workers"}}
	tests := []struct {
		name    string
		objects []any
		handler http.HandlerFunc
		reason  string // a NotStartedError's reason; empty means ErrNotFound
	}{
		{"no job", nil, never, ""},
		{"job without a pod yet", []any{job}, never, "NoPod"},
		{"container waiting", []any{podWithWorker("w", corev1.PodPending, waiting)}, never, "ImagePullBackOff"},
		{"no container status yet", []any{workerPod("w", corev1.PodPending)}, never, "Pending"},
		{"pod gone before the read", []any{podWithWorker("w", corev1.PodRunning, running)}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":"pods \"w-x7k2p\" not found"}`))
		}, ""},
		{"container not started at read time", []any{podWithWorker("w", corev1.PodRunning, running)}, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"BadRequest","code":400,"message":"container \"worker\" in pod \"w-x7k2p\" is waiting to start: ContainerCreating"}`))
		}, "ContainerCreating"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			j := newLogJobs(t, tt.handler, tt.objects...)
			_, err := j.Logs(t.Context(), "w", LogQuery{Tail: 10})
			var ns *NotStartedError
			switch {
			case tt.reason == "" && !errors.Is(err, ErrNotFound):
				t.Errorf("err = %v, want ErrNotFound", err)
			case tt.reason != "" && (!errors.As(err, &ns) || ns.Reason != tt.reason):
				t.Errorf("err = %v, want a NotStartedError with reason %s", err, tt.reason)
			}
		})
	}
}
