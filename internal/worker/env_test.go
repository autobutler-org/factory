package worker

import (
	"slices"
	"testing"
)

func TestHarnessEnvKeepsTheModelKeyAndDropsTheWorkersSecrets(t *testing.T) {
	environ := []string{
		"PATH=/bin", "HOME=/home/a", "XAI_API_KEY=xai-secret", "ANTHROPIC_API_KEY=ant-secret",
		"FACTORY_API_TOKEN=factory-secret", "FACTORY_TOKEN_FILE=/var/run/factory/token", "AWS_SECRET_ACCESS_KEY=aws-secret",
	}
	want := []string{"PATH=/bin", "HOME=/home/a", "XAI_API_KEY=xai-secret", "ANTHROPIC_API_KEY=ant-secret"}
	if got := harnessEnv(environ); !slices.Equal(got, want) {
		t.Errorf("harnessEnv = %q, want %q", got, want)
	}
}

func TestGateEnvKeepsTheBaseAndPointsHomeAtScratch(t *testing.T) {
	environ := []string{
		"PATH=/bin", "HOME=/home/a", "TMPDIR=/tmp", "SSL_CERT_FILE=/ca.pem", "LC_ALL=C", "XDG_CONFIG_HOME=/home/a/.config",
		"XDG_RUNTIME_DIR=/run/user/1", "XAI_API_KEY=xai-secret", "ANTHROPIC_API_KEY=ant-secret",
		"FACTORY_API_TOKEN=factory-secret", "AWS_SECRET_ACCESS_KEY=aws-secret",
	}
	want := []string{"PATH=/bin", "SSL_CERT_FILE=/ca.pem", "LC_ALL=C", "XDG_RUNTIME_DIR=/run/user/1", "HOME=/scratch", "TMPDIR=/scratch"}
	if got := gateEnv(environ, "/scratch"); !slices.Equal(got, want) {
		t.Errorf("gateEnv = %q, want %q", got, want)
	}
	if environ[1] != "HOME=/home/a" {
		t.Errorf("gateEnv changed its input: %q", environ)
	}
}
