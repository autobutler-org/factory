package work_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/work"
)

func TestRegisterPodBindsOnce(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	in := work.WorkerInput{Name: "ignored", Kind: work.WorkerLocal, Capabilities: work.Capabilities{Harnesses: []string{"claude-code"}}}
	pod := work.Pod{Sandbox: "worker-a1", Key: "item-1/o/r", UID: "uid-1"}

	w, cred, err := svc.RegisterPod(ctx, in, pod)
	if err != nil {
		t.Fatalf("RegisterPod: %v", err)
	}
	if w.Name != "worker-a1" || w.Kind != work.WorkerCluster {
		t.Errorf("worker = %s %s, want the sandbox's name, kind cluster, whatever the worker asked for", w.Name, w.Kind)
	}
	if !strings.HasPrefix(cred, work.CredentialPrefix) || len(cred) < 40 {
		t.Errorf("credential = %q, want a long random %s... secret", cred, work.CredentialPrefix)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT credential_hash FROM worker_credentials WHERE worker_id = $1`, w.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), cred) || len(stored) != 32 {
		t.Errorf("stored credential is %d bytes, want only its sha256", len(stored))
	}

	got, err := svc.WorkerByCredential(ctx, cred)
	if err != nil || got != (work.WorkerPrincipal{ID: w.ID, SandboxKey: "item-1/o/r"}) {
		t.Errorf("WorkerByCredential = %+v, %v; want worker-a1 bound to item-1/o/r", got, err)
	}

	// The same pod cannot register again: a session that reads the token
	// file after the worker registered gets nothing from it.
	if _, _, err := svc.RegisterPod(ctx, in, pod); !errors.Is(err, work.ErrAlreadyRegistered) {
		t.Errorf("second RegisterPod from the same pod = %v, want ErrAlreadyRegistered", err)
	}
	if _, err := svc.WorkerByCredential(ctx, cred); err != nil {
		t.Errorf("the refused registration revoked the first credential: %v", err)
	}

	// A new pod for the same sandbox replaces the binding and the credential.
	_, cred2, err := svc.RegisterPod(ctx, in, work.Pod{Sandbox: "worker-a1", Key: "item-1/o/r", UID: "uid-2"})
	if err != nil {
		t.Fatalf("RegisterPod from a new pod: %v", err)
	}
	if _, err := svc.WorkerByCredential(ctx, cred); !errors.Is(err, work.ErrNotFound) {
		t.Errorf("old pod's credential = %v, want ErrNotFound", err)
	}
	if _, err := svc.WorkerByCredential(ctx, cred2); err != nil {
		t.Errorf("new pod's credential: %v", err)
	}

	for _, bad := range []string{"", work.CredentialPrefix, work.CredentialPrefix + "forged", cred2 + "x"} {
		if _, err := svc.WorkerByCredential(ctx, bad); !errors.Is(err, work.ErrNotFound) {
			t.Errorf("WorkerByCredential(%q) = %v, want ErrNotFound", bad, err)
		}
	}

	// A worker marked gone has lost its credential.
	if _, err := pool.Exec(ctx, `UPDATE workers SET state = 'gone' WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.WorkerByCredential(ctx, cred2); !errors.Is(err, work.ErrNotFound) {
		t.Errorf("gone worker's credential = %v, want ErrNotFound", err)
	}
}

func TestRegisterPodValidates(t *testing.T) {
	svc, _ := newService(t)
	for _, pod := range []work.Pod{
		{Key: "k", UID: "u"},
		{Sandbox: "s", UID: "u"},
		{Sandbox: "s", Key: "k"},
	} {
		if _, _, err := svc.RegisterPod(context.Background(), work.WorkerInput{}, pod); !errors.Is(err, work.ErrInvalid) {
			t.Errorf("RegisterPod(%+v) = %v, want ErrInvalid", pod, err)
		}
	}
}
