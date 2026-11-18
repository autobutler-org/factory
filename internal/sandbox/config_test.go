package sandbox

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestParseLabels(t *testing.T) {
	t.Parallel()
	got, err := ParseLabels(" factory.autobutler.org/workers=true, kubernetes.io/arch=arm64 ")
	want := map[string]string{"factory.autobutler.org/workers": "true", "kubernetes.io/arch": "arm64"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseLabels = %v, %v; want %v", got, err, want)
	}
	if got, err := ParseLabels(""); err != nil || got != nil {
		t.Errorf("ParseLabels(\"\") = %v, %v; want nil", got, err)
	}
	for _, bad := range []string{"novalue", "=v", "a=1,a=2"} {
		if _, err := ParseLabels(bad); err == nil {
			t.Errorf("ParseLabels(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseTolerations(t *testing.T) {
	t.Parallel()
	got, err := ParseTolerations("factory.autobutler.org/workers=true:NoSchedule,dedicated:NoExecute")
	want := []corev1.Toleration{
		{Key: "factory.autobutler.org/workers", Operator: corev1.TolerationOpEqual, Value: "true", Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseTolerations = %+v, %v; want %+v", got, err, want)
	}
	if got, err := ParseTolerations(""); err != nil || got != nil {
		t.Errorf("ParseTolerations(\"\") = %v, %v; want nil", got, err)
	}
	for _, bad := range []string{"key=v", "key=v:Sometimes", ":NoSchedule"} {
		if _, err := ParseTolerations(bad); err == nil {
			t.Errorf("ParseTolerations(%q) succeeded, want an error", bad)
		}
	}
}

func TestParseResources(t *testing.T) {
	t.Parallel()
	got, err := ParseResources("cpu=1, memory=2Gi,ephemeral-storage=10Gi")
	want := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("2Gi"),
		corev1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
	}
	if err != nil || len(got) != 3 {
		t.Fatalf("ParseResources = %v, %v", got, err)
	}
	for k, q := range want {
		if g := got[k]; g.Cmp(q) != 0 {
			t.Errorf("%s = %s, want %s", k, g.String(), q.String())
		}
	}
	for _, bad := range []string{"gpu=1", "cpu=lots", "cpu"} {
		if _, err := ParseResources(bad); err == nil {
			t.Errorf("ParseResources(%q) succeeded, want an error", bad)
		}
	}
}
