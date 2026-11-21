package main

import (
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// TestWorkerJobEnvironmentIsWhatTheWorkerReads checks every variable the
// launcher's Job gives the worker against what factory worker reads: a
// FACTORY_<FLAG> for one of its flags, or FACTORY_API_TOKEN. The model key
// is for the agent CLI, not the worker.
func TestWorkerJobEnvironmentIsWhatTheWorkerReads(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	jobs, err := sandbox.NewJobs(client, &rest.Config{Host: "https://kube.test"}, sandbox.JobsConfig{
		Namespace: "factory-workers", Image: "w:1", ServerURL: "http://factory:80",
		APITokenSecret: "factory-worker-api-token", ModelKeySecret: "factory-model-api-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Ensure(t.Context(), "worker-a", "item/o/r", ""); err != nil {
		t.Fatal(err)
	}
	job, err := client.BatchV1().Jobs("factory-workers").Get(t.Context(), "worker-a", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if strings.Join(c.Args, " ") != "worker" {
		t.Errorf("args = %v, want [worker]", c.Args)
	}
	fs, _ := workerCommand().flags()
	for _, e := range c.Env {
		switch {
		case e.Name == apiTokenEnv, e.Name == "ANTHROPIC_API_KEY":
			continue
		case !strings.HasPrefix(e.Name, "FACTORY_"):
			t.Errorf("%s is not a factory worker setting", e.Name)
			continue
		}
		flagName := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(e.Name, "FACTORY_"), "_", "-"))
		if envName(flagName) != e.Name || fs.Lookup(flagName) == nil {
			t.Errorf("the Job sets %s, but factory worker has no --%s flag", e.Name, flagName)
		}
	}
}
