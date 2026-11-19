package sandbox

import (
	"errors"
	"testing"
)

func TestAllowImage(t *testing.T) {
	t.Parallel()
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	allowed := []string{"ghcr.io/autobutler-org/", "registry.test:5000/tools/flutter"}
	for _, tc := range []struct {
		ref  string
		want error
	}{
		{"ghcr.io/autobutler-org/quark-worker:1.2.3", nil},
		{"ghcr.io/autobutler-org/team/quark-worker:v1", nil},
		{"ghcr.io/autobutler-org/quark-worker@" + digest, nil},
		{"ghcr.io/autobutler-org/quark-worker:1.2.3@" + digest, nil},
		{"registry.test:5000/tools/flutter:3.24", nil},
		// A prefix without a trailing slash is one repository, not a prefix.
		{"registry.test:5000/tools/flutter-evil:3.24", ErrImageNotAllowed},
		{"ghcr.io/autobutler-org.evil.example/x:1", ErrImageNotAllowed},
		{"ghcr.io/autobutler-orgx/x:1", ErrImageNotAllowed},
		{"evil.example/ghcr.io/autobutler-org/x:1", ErrImageNotAllowed},
		{"ghcr.io/other/x:1", ErrImageNotAllowed},
		{"ghcr.io/autobutler-org/quark-worker", ErrBadImage},     // no tag or digest
		{"autobutler-org/quark-worker:1", ErrBadImage},           // no registry host
		{"ghcr.io/autobutler-org/../x:1", ErrBadImage},           // not a path component
		{"ghcr.io/autobutler-org/Quark:1", ErrBadImage},          // upper case
		{"ghcr.io/autobutler-org/x@sha256:abc", ErrBadImage},     // short digest
		{"ghcr.io/autobutler-org/x:1 --privileged", ErrBadImage}, // not one reference
		{"", ErrBadImage},
	} {
		t.Run(tc.ref, func(t *testing.T) {
			t.Parallel()
			if err := AllowImage(tc.ref, allowed); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Errorf("AllowImage(%q) = %v, want %v", tc.ref, err, tc.want)
			}
		})
	}
	if err := AllowImage("ghcr.io/autobutler-org/x:1", nil); !errors.Is(err, ErrImageNotAllowed) {
		t.Errorf("AllowImage with an empty allowlist = %v, want ErrImageNotAllowed", err)
	}
}
