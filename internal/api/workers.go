package api

import (
	"net/http"

	"github.com/autobutler-org/factory/internal/work"
)

func (h handlers) registerWorker(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string            `json:"name"`
		Kind         string            `json:"kind"`
		Capabilities work.Capabilities `json:"capabilities"`
		Version      string            `json:"version"`
	}
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	worker, err := h.cfg.Work.RegisterWorker(r.Context(), work.WorkerInput{Name: req.Name, Kind: req.Kind, Capabilities: req.Capabilities, Version: req.Version})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, worker)
}

func (h handlers) listWorkers(w http.ResponseWriter, r *http.Request) {
	workers, err := h.cfg.Work.Workers(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"workers": workers})
}

func (h handlers) heartbeat(w http.ResponseWriter, r *http.Request) {
	held, err := h.cfg.Work.Heartbeat(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"held": held, "lease_seconds": h.cfg.Work.Policy().Lease.Seconds()})
}

func (h handlers) claimNext(w http.ResponseWriter, r *http.Request) {
	item, ok, err := h.cfg.Work.ClaimNext(r.Context(), r.PathValue("id"))
	switch {
	case err != nil:
		h.fail(w, r, err)
	case !ok:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, item)
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
	h.respondItem(w, r)(h.cfg.Work.ReportStage(r.Context(), r.PathValue("id"), req.WorkerID, req.Stage, req.Outputs))
}

func (h handlers) result(w http.ResponseWriter, r *http.Request) {
	var req workerRequest
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
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
