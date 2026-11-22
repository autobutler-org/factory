package work

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/jackc/pgx/v5"
)

// CredentialPrefix starts every worker credential, so a bearer token can be
// told apart from the shared API token and from a ServiceAccount token.
const CredentialPrefix = "fwc_"

// ErrAlreadyRegistered is a pod registering a second time. A pod registers
// once, so its token file is worth nothing once its worker has registered.
var ErrAlreadyRegistered = errors.New("this pod has already registered; a worker registers once per pod, so start a new sandbox")

// Pod is the sandbox pod a worker proved it runs in (sandbox.Identity).
type Pod struct {
	// Sandbox is the Job's name, which the worker registers under.
	Sandbox string
	// Key is the sandbox key the launcher gave the Job.
	Key string
	// UID is the pod's uid.
	UID string
}

// WorkerPrincipal is the worker a credential belongs to, and the only chain
// it may claim from.
type WorkerPrincipal struct {
	ID         string
	SandboxKey string
}

// RegisterPod registers the worker of a sandbox pod and returns it with a
// new credential, which is returned only here and stored only as its
// SHA-256. The name is the sandbox's and the kind is cluster, whatever in
// says; in supplies the capabilities and version. A pod that has registered
// before gets ErrAlreadyRegistered and its first credential stays valid; a
// new pod of the same sandbox replaces it.
func (s *Service) RegisterPod(ctx context.Context, in WorkerInput, pod Pod) (db.Worker, string, error) {
	if pod.Sandbox == "" || pod.Key == "" || pod.UID == "" {
		return db.Worker{}, "", fmt.Errorf("%w: a pod registration needs the sandbox, its key and the pod uid", ErrInvalid)
	}
	in.Name, in.Kind = pod.Sandbox, WorkerCluster
	params, err := in.params()
	if err != nil {
		return db.Worker{}, "", err
	}
	credential := CredentialPrefix + rand.Text() + rand.Text()
	hash := sha256.Sum256([]byte(credential))
	var w db.Worker
	err = s.inTx(ctx, func(q *db.Queries) error {
		var err error
		if w, err = q.RegisterWorker(ctx, params); err != nil {
			return fmt.Errorf("register worker: %w", err)
		}
		_, err = q.BindWorkerCredential(ctx, db.BindWorkerCredentialParams{
			WorkerID: w.ID, PodUid: pod.UID, SandboxKey: pod.Key, CredentialHash: hash[:],
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("pod %s of %s: %w", pod.UID, pod.Sandbox, ErrAlreadyRegistered)
		}
		if err != nil {
			return fmt.Errorf("bind the worker to its pod: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.Worker{}, "", err
	}
	return w, credential, nil
}

// WorkerByCredential returns the worker a credential belongs to, or
// ErrNotFound for a credential that is unknown, replaced by a newer pod's,
// or held by a worker marked gone.
func (s *Service) WorkerByCredential(ctx context.Context, credential string) (WorkerPrincipal, error) {
	hash := sha256.Sum256([]byte(credential))
	row, err := db.New(s.pool).WorkerByCredential(ctx, hash[:])
	if err != nil {
		return WorkerPrincipal{}, notFound(err, "worker credential")
	}
	return WorkerPrincipal{ID: row.WorkerID, SandboxKey: row.SandboxKey}, nil
}
