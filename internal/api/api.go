// Package api is factory's HTTP JSON API: /healthz, /readyz and the versioned
// routes under /api/v1, including read-only views of a work item's worker
// sandboxes and their logs through Config.Sandboxes. Every other path goes to Config.UI, the web UI, when
// one is set. Handlers only decode the request, call the work service, and
// encode the result; they hold no SQL and no state rules.
//
// Every error has one shape, {"error": {"code", "message", "request_id", ...}},
// with a stable machine-readable code. The package also adds request IDs,
// access logs, a request body limit and, when a token is configured, bearer
// token authentication on /api/v1.
//
// With Config.IdentifyWorker set (workload identity, #15), a cluster worker
// registers with its pod's projected ServiceAccount token and gets a
// credential bound to its worker and sandbox key. That credential reaches
// only the worker protocol routes, only for its own worker and chain, and
// the shared token is refused wherever it would act as a worker. People and
// the CLI still share the one token until the people half of #15. The
// package does not listen or manage the server's lifetime; cmd/factory does.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/doctor"
	"github.com/autobutler-org/factory/internal/lease"
	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/autobutler-org/factory/internal/work"
)

// MaxBodyBytes caps every request body.
const MaxBodyBytes = 1 << 20

// Config is what the API needs.
type Config struct {
	Work *work.Service
	// Ready reports whether the server can serve: the database is reachable
	// and migrated. /readyz returns its error.
	Ready func(context.Context) error
	// Token, when set, is required as "Authorization: Bearer <token>" on
	// every /api/v1 route.
	//
	// ponytail: one shared token, so every caller is the same identity and
	// events record actor "api". Real identities and scoped tokens are #15.
	Token  string
	Logger *slog.Logger
	// MintToken mints a code host token for one repository. Nil means no
	// GitHub App is configured, and codehost-token requests get 503.
	MintToken func(ctx context.Context, repo string) (codehost.Token, error)
	// CommitIdentity is who workers commit as, sent beside each code host
	// token. Nil, or a failed lookup, sends none, and the worker commits as
	// its built-in factory identity.
	CommitIdentity func(ctx context.Context) (codehost.Identity, error)
	// Version is the server's version, which GET /api/v1/admin/doctor
	// reports so a client can compare it with its own.
	Version string
	// Doctor runs the server-side checks GET /api/v1/admin/doctor reports.
	// Nil reports none.
	Doctor func(context.Context) []doctor.Result
	// UI, when set, serves every path outside /api/v1, /healthz and /readyz,
	// without the token: the web UI asks for the token itself. Nil answers
	// those paths with a JSON 404.
	UI http.Handler
	// IdentifyWorker, when set, turns workload identity on: it verifies a
	// worker pod's projected ServiceAccount token (sandbox.Jobs.Identify)
	// when the worker registers. Nil leaves workers on Token, for a worker
	// outside Kubernetes against a server without the launcher.
	IdentifyWorker func(ctx context.Context, token string) (sandbox.Identity, error)
	// Sandboxes is the runtime the launcher starts worker sandboxes with,
	// read for the sandbox routes. Nil means the launcher is off, and those
	// routes get 503.
	Sandboxes sandbox.Runtime
}

type ctxKey struct{}

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// New returns the API's handler.
func New(cfg Config) http.Handler {
	h := handlers{cfg: cfg}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", h.readyz)
	if cfg.UI != nil {
		mux.Handle("/", cfg.UI)
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			writeError(w, r, http.StatusNotFound, "not_found", "no route for "+r.Method+" "+r.URL.Path, nil)
		})
	}

	v1 := http.NewServeMux()
	v1.HandleFunc("GET /api/v1/work", h.listWork)
	v1.HandleFunc("POST /api/v1/work", h.enqueue)
	v1.HandleFunc("GET /api/v1/work/{id}", h.getWork)
	v1.HandleFunc("PATCH /api/v1/work/{id}", h.patchWork)
	v1.HandleFunc("POST /api/v1/work/{id}/transitions", h.transition)
	v1.HandleFunc("GET /api/v1/work/{id}/events", h.events)
	v1.HandleFunc("GET /api/v1/work/{id}/outputs", h.outputs)
	v1.HandleFunc("PUT /api/v1/work/{id}/dependencies/{blocked_by_id}", h.putDependency)
	v1.HandleFunc("DELETE /api/v1/work/{id}/dependencies/{blocked_by_id}", h.deleteDependency)
	v1.HandleFunc("POST /api/v1/work/{id}/claim", h.claimItem)
	v1.HandleFunc("POST /api/v1/work/{id}/release", h.release)
	v1.HandleFunc("POST /api/v1/work/{id}/stage", h.stage)
	v1.HandleFunc("POST /api/v1/work/{id}/result", h.result)
	v1.HandleFunc("POST /api/v1/work/{id}/outputs", h.recordOutput)
	v1.HandleFunc("POST /api/v1/work/{id}/codehost-token", h.codehostToken)
	v1.HandleFunc("POST /api/v1/workers", h.registerWorker)
	v1.HandleFunc("GET /api/v1/workers", h.listWorkers)
	v1.HandleFunc("GET /api/v1/status", h.status)
	v1.HandleFunc("GET /api/v1/admin/doctor", h.doctor)
	v1.HandleFunc("POST /api/v1/workers/{id}/heartbeat", h.heartbeat)
	v1.HandleFunc("POST /api/v1/workers/{id}/claim", h.claimStep)
	v1.HandleFunc("GET /api/v1/work/{id}/graph", h.graph)
	v1.HandleFunc("GET /api/v1/work/{id}/sandboxes", h.listSandboxes)
	v1.HandleFunc("GET /api/v1/work/{id}/sandboxes/{name}/logs", h.sandboxLogs)
	v1.HandleFunc("POST /api/v1/work/{id}/replan", h.replan)
	v1.HandleFunc("POST /api/v1/steps/{id}/stage", h.stepStage)
	v1.HandleFunc("POST /api/v1/steps/{id}/result", h.stepResult)
	v1.HandleFunc("POST /api/v1/steps/{id}/release", h.stepRelease)
	v1.HandleFunc("POST /api/v1/steps/{id}/complete", h.completeWait)
	v1.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "no route for "+r.Method+" "+r.URL.Path, nil)
	})

	// The only routes a worker credential reaches.
	workers := http.NewServeMux()
	workers.HandleFunc("POST /api/v1/workers/{id}/heartbeat", h.heartbeat)
	workers.HandleFunc("POST /api/v1/workers/{id}/claim", h.claimStep)
	workers.HandleFunc("POST /api/v1/steps/{id}/stage", h.stepStage)
	workers.HandleFunc("POST /api/v1/steps/{id}/result", h.stepResult)
	workers.HandleFunc("POST /api/v1/steps/{id}/release", h.stepRelease)
	workers.HandleFunc("POST /api/v1/work/{id}/codehost-token", h.codehostToken)
	workers.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusForbidden, "forbidden", "a worker credential may only heartbeat, claim, report on its own steps "+
			"and get code host tokens for them; "+r.Method+" "+r.URL.Path+" is not one of those", nil)
	})
	mux.Handle("/api/v1/", h.authenticate(v1, workers))

	return h.observe(mux)
}

type handlers struct {
	cfg Config
}

// observe assigns a request ID, limits the body, and writes an access log.
func (h handlers) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if !safeRequestID.MatchString(id) {
			raw := make([]byte, 8)
			_, _ = rand.Read(raw)
			id = hex.EncodeToString(raw)
		}
		w.Header().Set("X-Request-ID", id)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		h.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("request_id", id),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rec.status),
			slog.Int("bytes", rec.bytes),
			slog.Duration("duration", time.Since(start)),
			slog.String("remote", r.RemoteAddr),
		)
	})
}

type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// authenticate sends a request to people or, for a worker credential, to
// workers. The shared token is required when one is configured; both sides
// are hashed first so the comparison takes the same time whatever the
// length. With workload identity on, a worker credential (work.CredentialPrefix)
// is looked up, and any other bearer on POST /api/v1/workers is taken as a
// pod's ServiceAccount token and verified.
func (h handlers) authenticate(people, workers http.Handler) http.Handler {
	want := sha256.Sum256([]byte(h.cfg.Token))
	identity := h.cfg.IdentifyWorker != nil
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, hasBearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		hasBearer = hasBearer && presented != ""
		got := sha256.Sum256([]byte(presented))
		shared := h.cfg.Token != "" && hasBearer && subtle.ConstantTimeCompare(got[:], want[:]) == 1
		switch {
		case identity && hasBearer && strings.HasPrefix(presented, work.CredentialPrefix):
			p, err := h.cfg.Work.WorkerByCredential(r.Context(), presented)
			if errors.Is(err, work.ErrNotFound) {
				writeError(w, r, http.StatusUnauthorized, "unauthorized", "the worker credential is unknown, was replaced by a newer pod's, "+
					"or its worker was marked gone. A worker registers once per pod: let this sandbox end and the launcher start a new one", nil)
				return
			}
			if err != nil {
				h.fail(w, r, err)
				return
			}
			workers.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
		case identity && hasBearer && !shared && r.Method == http.MethodPost && r.URL.Path == "/api/v1/workers":
			h.registerPod(w, r, presented)
		case shared || h.cfg.Token == "":
			people.ServeHTTP(w, r)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="factory"`)
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "send the API token as \"Authorization: Bearer <token>\"", nil)
		}
	})
}

type principalKey struct{}

// actAs reports whether the caller may act as the worker workerID, and
// answers the request when it may not. A worker credential acts only as its
// own worker. Without one, the caller holds the shared token (or the server
// has none), which acts as any worker unless workload identity is on.
func (h handlers) actAs(w http.ResponseWriter, r *http.Request, workerID string) bool {
	if p, ok := r.Context().Value(principalKey{}).(work.WorkerPrincipal); ok {
		if workerID == p.ID {
			return true
		}
		writeError(w, r, http.StatusForbidden, "forbidden", fmt.Sprintf("this credential is worker %s's; it cannot act for worker %q", p.ID, workerID), nil)
		return false
	}
	if h.cfg.IdentifyWorker == nil {
		return true
	}
	writeError(w, r, http.StatusForbidden, "workload_identity_required", "the shared API token cannot act as a worker while the launcher is on: "+
		"a worker in the cluster registers with its pod's projected ServiceAccount token ("+sandbox.TokenPath+"). "+
		"To run a worker outside Kubernetes, point it at a server without --launcher", nil)
	return false
}

// doctor is GET /api/v1/admin/doctor: the server's version and its checks.
// A failing check is still a 200; the checks are the answer.
func (h handlers) doctor(w http.ResponseWriter, r *http.Request) {
	checks := []doctor.Result{}
	if h.cfg.Doctor != nil {
		checks = append(checks, h.cfg.Doctor(r.Context())...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": h.cfg.Version, "checks": checks})
}

func (h handlers) readyz(w http.ResponseWriter, r *http.Request) {
	if err := h.cfg.Ready(r.Context()); err != nil {
		writeError(w, r, http.StatusServiceUnavailable, "not_ready", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// actor is who API calls are recorded as. With one shared token there is no
// caller identity to record yet (#15).
func apiActor() work.Actor { return work.Actor{Kind: work.ActorUser, ID: "api"} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, extra map[string]any) {
	body := map[string]any{"code": code, "message": message}
	if id, ok := r.Context().Value(ctxKey{}).(string); ok {
		body["request_id"] = id
	}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, map[string]any{"error": body})
}

// fail maps a service error to its status and code.
func (h handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	var te *work.TransitionError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &te):
		writeError(w, r, http.StatusConflict, "illegal_transition", err.Error(), map[string]any{"current_state": te.From, "allowed": te.Allowed})
	case errors.As(err, &tooLarge):
		writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large", fmt.Sprintf("request body is larger than %d bytes", MaxBodyBytes), nil)
	case errors.Is(err, errMalformed):
		writeError(w, r, http.StatusBadRequest, "malformed_request", err.Error(), nil)
	case errors.Is(err, work.ErrInvalid):
		writeError(w, r, http.StatusUnprocessableEntity, "invalid_request", err.Error(), nil)
	case errors.Is(err, work.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, work.ErrUnfinishedDependencies):
		writeError(w, r, http.StatusConflict, "unfinished_dependencies", err.Error(), nil)
	case errors.Is(err, work.ErrOutputsPending):
		writeError(w, r, http.StatusConflict, "outputs_pending", err.Error(), nil)
	case errors.Is(err, work.ErrNoOutputs):
		writeError(w, r, http.StatusConflict, "no_outputs", err.Error(), nil)
	case errors.Is(err, lease.ErrNotClaimant):
		writeError(w, r, http.StatusConflict, "not_claimant", err.Error(), nil)
	case errors.Is(err, lease.ErrExpired):
		writeError(w, r, http.StatusConflict, "lease_expired", err.Error(), nil)
	case errors.Is(err, work.ErrWorkerNotActive):
		writeError(w, r, http.StatusConflict, "worker_not_active", err.Error(), nil)
	case errors.Is(err, work.ErrNotWaiting):
		writeError(w, r, http.StatusConflict, "not_waiting", err.Error(), nil)
	case errors.Is(err, work.ErrNotReplannable):
		writeError(w, r, http.StatusConflict, "not_replannable", err.Error(), nil)
	case errors.Is(err, work.ErrOutputConflict):
		writeError(w, r, http.StatusConflict, "output_conflict", err.Error(), nil)
	case errors.Is(err, work.ErrConfigUnavailable):
		writeError(w, r, http.StatusServiceUnavailable, "config_unavailable", err.Error(), nil)
	case errors.Is(err, work.ErrAlreadyRegistered):
		writeError(w, r, http.StatusConflict, "already_registered", err.Error(), nil)
	case errors.Is(err, work.ErrRepoNotAllowed):
		writeError(w, r, http.StatusForbidden, "repo_not_allowed", err.Error(), nil)
	default:
		h.cfg.Logger.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "err", err)
		writeError(w, r, http.StatusInternalServerError, "internal", "internal error; the server log has the details", nil)
	}
}

var errMalformed = errors.New("malformed request body")

// decode reads a JSON body into v, refusing unknown fields and trailing data.
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return fmt.Errorf("%w: %w", errMalformed, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected data after the JSON object", errMalformed)
	}
	return nil
}
