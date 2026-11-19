package repoconfig

import (
	"os"
	"testing"
)

// TestOwnConfig pins that this repository's own .factory/config.toml is one
// the factory accepts, so a bad edit fails here and not on the next item.
func TestOwnConfig(t *testing.T) {
	const name = "../../.factory/config.toml"
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse(name, data)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gate != "make check" {
		t.Errorf("gate = %q, want make check", cfg.Gate)
	}
}
