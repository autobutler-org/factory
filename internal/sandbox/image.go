package sandbox

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	// ErrBadImage is returned for an image reference that is not a full
	// reference: registry host, repository, and a tag or a digest.
	ErrBadImage = errors.New("malformed image reference")
	// ErrImageNotAllowed is returned for an image whose repository is under
	// none of the allowed prefixes.
	ErrImageNotAllowed = errors.New("image is not allowed")
)

// imageRef is the reference grammar this package accepts: a registry host
// (with a dot or a port, or localhost), lower-case path components, then a
// tag, a digest, or both. It is stricter than the distribution grammar on
// purpose: no implicit Docker Hub, no implicit latest.
var imageRef = regexp.MustCompile(`^((?:[a-z0-9-]+(?:\.[a-z0-9-]+)+(?::[0-9]+)?|[a-z0-9-]+:[0-9]+|localhost)` +
	`(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)+)` +
	`(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?` +
	`(?:@[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-f0-9]{32,})?$`)

// AllowImage says whether a sandbox may run ref, given the allowed
// repository prefixes. ref must be a full reference with a tag or digest.
// Matching is on its repository (the reference without tag or digest): a
// prefix ending in "/" allows every repository under it, and one without
// allows exactly that repository. So "ghcr.io/autobutler-org/" allows
// "ghcr.io/autobutler-org/quark-worker:1" but not
// "ghcr.io/autobutler-org.evil.example/x:1". No prefixes allows nothing.
func AllowImage(ref string, allowed []string) error {
	m := imageRef.FindStringSubmatch(ref)
	if m == nil || len(m[0]) == len(m[1]) {
		return fmt.Errorf("%w: %q; write registry/repository:tag or registry/repository@sha256:<digest>", ErrBadImage, ref)
	}
	repo := m[1]
	for _, prefix := range allowed {
		if repo == prefix || (strings.HasSuffix(prefix, "/") && strings.HasPrefix(repo, prefix)) {
			return nil
		}
	}
	if len(allowed) == 0 {
		return fmt.Errorf("%w: %s: this server lets no repository choose its image (--launcher-allowed-images is empty)", ErrImageNotAllowed, ref)
	}
	return fmt.Errorf("%w: %s: its repository %s is under none of %s (--launcher-allowed-images)", ErrImageNotAllowed, ref, repo, strings.Join(allowed, ", "))
}
