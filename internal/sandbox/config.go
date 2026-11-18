package sandbox

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// ErrBadList is returned for a comma-separated setting that cannot be parsed.
var ErrBadList = errors.New("malformed list")

// pairs splits "a=1, b=2" into its key-value pairs. Empty input is none.
func pairs(s string) ([][2]string, error) {
	var out [][2]string
	for item := range strings.SplitSeq(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		k, v, ok := strings.Cut(item, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%w: %q is not key=value", ErrBadList, item)
		}
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

// ParseLabels parses "key=value,key=value" into a map, as for a node
// selector. Empty input is nil.
func ParseLabels(s string) (map[string]string, error) {
	ps, err := pairs(s)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	out := make(map[string]string, len(ps))
	for _, p := range ps {
		if _, dup := out[p[0]]; dup {
			return nil, fmt.Errorf("%w: %q is given twice", ErrBadList, p[0])
		}
		out[p[0]] = p[1]
	}
	return out, nil
}

// ParseTolerations parses "key=value:Effect" (operator Equal) and
// "key:Effect" (operator Exists), separated by commas, as kubectl taint
// writes taints. Effect is NoSchedule, PreferNoSchedule or NoExecute. Empty
// input is nil.
func ParseTolerations(s string) ([]corev1.Toleration, error) {
	var out []corev1.Toleration
	for item := range strings.SplitSeq(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		kv, effect, ok := strings.Cut(item, ":")
		switch corev1.TaintEffect(effect) {
		case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
		default:
			ok = false
		}
		key, value, hasValue := strings.Cut(kv, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%w: toleration %q is not key=value:Effect or key:Effect, with Effect NoSchedule, PreferNoSchedule or NoExecute", ErrBadList, item)
		}
		t := corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffect(effect)}
		if hasValue {
			t.Operator, t.Value = corev1.TolerationOpEqual, value
		}
		out = append(out, t)
	}
	return out, nil
}

// ParseResources parses "cpu=1,memory=2Gi,ephemeral-storage=10Gi" into a
// resource list. Only cpu, memory and ephemeral-storage are accepted.
func ParseResources(s string) (corev1.ResourceList, error) {
	ps, err := pairs(s)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	out := corev1.ResourceList{}
	for _, p := range ps {
		name := corev1.ResourceName(p[0])
		if name != corev1.ResourceCPU && name != corev1.ResourceMemory && name != corev1.ResourceEphemeralStorage {
			return nil, fmt.Errorf("%w: resource %q is not cpu, memory or ephemeral-storage", ErrBadList, p[0])
		}
		q, err := resource.ParseQuantity(p[1])
		if err != nil {
			return nil, fmt.Errorf("%w: %s=%q: %w", ErrBadList, p[0], p[1], err)
		}
		out[name] = q
	}
	return out, nil
}
