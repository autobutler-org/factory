// Package api is factory's HTTP JSON API: /healthz, /readyz and the versioned
// routes under /api/v1. Handlers only decode the request, call the work
// service, and encode the result; they hold no SQL and no state rules.
//
// Every error has one shape, {"error": {"code", "message", "request_id", ...}},
// with a stable machine-readable code. The package also adds request IDs,
// access logs, a request body limit and, when a token is configured, bearer
// token authentication on /api/v1. It does not listen or manage the server's
// lifetime; cmd/factory does.
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
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "no route for "+r.Method+" "+r.URL.Path, nil)
	})

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
	v1.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "not_found", "no route for "+r.Method+" "+r.URL.Path, nil)
	})
	mux.Handle("/api/v1/", h.authenticate(v1))

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

// authenticate requires the bearer token when one is configured. Both sides
// are hashed first so the comparison takes the same time whatever the length.
func (h handlers) authenticate(next http.Handler) http.Handler {
	if h.cfg.Token == "" {
		return next
	}
	want := sha256.Sum256([]byte(h.cfg.Token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		got := sha256.Sum256([]byte(presented))
		if !ok || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="factory"`)
			writeError(w, r, http.StatusUnauthorized, "unauthorized", "send the API token as \"Authorization: Bearer <token>\"", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
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
	case errors.Is(err, work.ErrOutputConflict):
		writeError(w, r, http.StatusConflict, "output_conflict", err.Error(), nil)
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
