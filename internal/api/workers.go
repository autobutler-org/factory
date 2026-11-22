package api

import (
	"errors"
	"net/http"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/autobutler-org/factory/internal/work"
)

type registerRequest struct {
	Name         string            `json:"name"`
	Kind         string            `json:"kind"`
	Capabilities work.Capabilities `json:"capabilities"`
	Version      string            `json:"version"`
}

func (h handlers) registerWorker(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.actAs(w, r, "") {
		return
	}
	worker, err := h.cfg.Work.RegisterWorker(r.Context(), work.WorkerInput{Name: req.Name, Kind: req.Kind, Capabilities: req.Capabilities, Version: req.Version})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

// registerPod is POST /api/v1/workers with a pod's projected ServiceAccount
// token. The worker is named after its sandbox and bound to the sandbox's
// key, whatever the body says; the reply carries the credential the worker
// sends from then on, once.
func (h handlers) registerPod(w http.ResponseWriter, r *http.Request, token string) {
	id, err := h.cfg.IdentifyWorker(r.Context(), token)
	if errors.Is(err, sandbox.ErrUnauthenticated) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="factory"`)
		writeError(w, r, http.StatusUnauthorized, "unauthorized", err.Error()+". A worker in the cluster registers with the projected "+
			"ServiceAccount token its Job mounts at "+sandbox.TokenPath+" (audience factory), read when it starts; "+
			"a worker outside Kubernetes needs a server without --launcher and the shared FACTORY_API_TOKEN", nil)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var req registerRequest
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	worker, credential, err := h.cfg.Work.RegisterPod(r.Context(), work.WorkerInput{Capabilities: req.Capabilities, Version: req.Version},
		work.Pod{Sandbox: id.Sandbox, Key: id.Key, UID: id.PodUID})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.cfg.Logger.InfoContext(r.Context(), "worker pod registered", "worker_id", worker.ID, "sandbox", id.Sandbox, "pod", id.Pod, "sandbox_key", id.Key)
	writeJSON(w, http.StatusOK, struct {
		db.Worker
		Credential string `json:"credential"`
	}{worker, credential})
}

func (h handlers) listWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := h.cfg.Work.Workers(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers})
}

func (h handlers) status(w http.ResponseWriter, r *http.Request) {
	st, err := h.cfg.Work.Status(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h handlers) heartbeat(w http.ResponseWriter, r *http.Request) {
	if !h.actAs(w, r, r.PathValue("id")) {
		return
	}
	held, err := h.cfg.Work.Heartbeat(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"held": held, "lease_seconds": h.cfg.Work.Policy().Lease.Seconds()})
}

// claimStep is POST /api/v1/workers/{id}/claim: the step protocol. The body
// is optional; {"sandbox_key": "..."} restricts the claim to one sandbox.
func (h handlers) claimStep(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SandboxKey string `json:"sandbox_key"`
	}
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	if !h.actAs(w, r, r.PathValue("id")) {
		return
	}
	// A worker credential claims only from its own sandbox's chain.
	if p, ok := r.Context().Value(principalKey{}).(work.WorkerPrincipal); ok {
		if req.SandboxKey != "" && req.SandboxKey != p.SandboxKey {
			writeError(w, r, http.StatusForbidden, "forbidden", "this worker serves sandbox key "+p.SandboxKey+"; it cannot claim from "+req.SandboxKey, nil)
			return
		}
		req.SandboxKey = p.SandboxKey
	}
	claim, ok, err := h.cfg.Work.ClaimStep(r.Context(), r.PathValue("id"), req.SandboxKey)
	switch {
	case err != nil:
		h.fail(w, r, err)
	case !ok:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, claim)
	}
}

type workerRequest struct {
	WorkerID  string             `json:"worker_id"`
	Reason    string             `json:"reason"`
	Outcome   string             `json:"outcome"`
	Retryable bool               `json:"retryable"`
	Outputs   []work.OutputInput `json:"outputs"`
}

func (h handlers) claimItem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID string `json:"worker_id"`
	}
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.actAs(w, r, req.WorkerID) {
		return
	}
	h.respondItem(w, r)(h.cfg.Work.Claim(r.Context(), r.PathValue("id"), req.WorkerID))
}

func (h handlers) release(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID string `json:"worker_id"`
		Reason   string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.actAs(w, r, req.WorkerID) {
		return
	}
	h.respondItem(w, r)(h.cfg.Work.Release(r.Context(), r.PathValue("id"), req.WorkerID, req.Reason))
}

func (h handlers) stage(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID string             `json:"worker_id"`
		Stage    string             `json:"stage"`
		Outputs  []work.OutputInput `json:"outputs"`
	}
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.actAs(w, r, req.WorkerID) {
		return
	}
	h.respondItem(w, r)(h.cfg.Work.ReportStage(r.Context(), r.PathValue("id"), req.WorkerID, req.Stage, req.Outputs))
}

func (h handlers) result(w http.ResponseWriter, r *http.Request) {
	var req workerRequest
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.actAs(w, r, req.WorkerID) {
		return
	}
	h.respondItem(w, r)(h.cfg.Work.ReportResult(r.Context(), r.PathValue("id"), req.WorkerID,
		work.Result{Outcome: req.Outcome, Reason: req.Reason, Retryable: req.Retryable, Outputs: req.Outputs}))
}

// recordOutput is POST /api/v1/work/{id}/outputs. With a worker_id it is the
// claimant recording what it pushed; without one it is an operator or an
// adapter updating an output, such as a pull request that merged.
func (h handlers) recordOutput(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID string `json:"worker_id"`
		work.OutputInput
	}
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	id := r.PathValue("id")
	var out any
	var err error
	if req.WorkerID != "" {
		if !h.actAs(w, r, req.WorkerID) {
			return
		}
		out, err = h.cfg.Work.RecordWorkerOutput(r.Context(), id, req.WorkerID, req.OutputInput)
	} else {
		out, err = h.cfg.Work.RecordOutput(r.Context(), id, req.OutputInput, apiActor())
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h handlers) respondItem(w http.ResponseWriter, r *http.Request) func(any, error) {
	return func(item any, err error) {
		if err != nil {
			h.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}
