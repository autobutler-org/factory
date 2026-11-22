package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/autobutler-org/factory/internal/work"
)

// stepOutcome is how a step ended, as a worker or an adapter reports it.
type stepOutcome struct {
	Outcome   string             `json:"outcome"`
	Result    json.RawMessage    `json:"result"`
	Reason    string             `json:"reason"`
	Retryable bool               `json:"retryable"`
	Outputs   []work.OutputInput `json:"outputs"`
}

func (req stepOutcome) result() work.StepResult {
	return work.StepResult{Outcome: req.Outcome, Result: req.Result, Reason: req.Reason, Retryable: req.Retryable, Outputs: req.Outputs}
}

func (h handlers) graph(w http.ResponseWriter, r *http.Request) {
	h.respondItem(w, r)(h.cfg.Work.Graph(r.Context(), r.PathValue("id")))
}

func (h handlers) replan(w http.ResponseWriter, r *http.Request) {
	h.respondItem(w, r)(h.cfg.Work.Replan(r.Context(), r.PathValue("id"), apiActor()))
}

func (h handlers) stepStage(w http.ResponseWriter, r *http.Request) {
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
	h.respondItem(w, r)(h.cfg.Work.ReportStepStage(r.Context(), r.PathValue("id"), req.WorkerID, req.Stage, req.Outputs))
}

func (h handlers) stepResult(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WorkerID string `json:"worker_id"`
		stepOutcome
	}
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.actAs(w, r, req.WorkerID) {
		return
	}
	h.respondItem(w, r)(h.cfg.Work.ReportStepResult(r.Context(), r.PathValue("id"), req.WorkerID, req.result()))
}

func (h handlers) stepRelease(w http.ResponseWriter, r *http.Request) {
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
	h.respondItem(w, r)(h.cfg.Work.ReleaseStep(r.Context(), r.PathValue("id"), req.WorkerID, req.Reason))
}

// completeWait is POST /api/v1/steps/{id}/complete: an adapter (#7) or an
// operator reports the event a wait step was waiting for.
func (h handlers) completeWait(w http.ResponseWriter, r *http.Request) {
	var req stepOutcome
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	actor := work.Actor{Kind: work.ActorAdapter, ID: "api"}
	h.respondItem(w, r)(h.cfg.Work.CompleteWait(r.Context(), r.PathValue("id"), req.result(), actor))
}

// putStepOutput is PUT /api/v1/steps/{id}/output?worker_id=: the claimant
// uploads a gate run's combined output as the raw body, at most
// work.MaxStepOutputBytes.
func (h handlers) putStepOutput(w http.ResponseWriter, r *http.Request) {
	workerID := r.URL.Query().Get("worker_id")
	if workerID == "" {
		writeError(w, r, http.StatusBadRequest, "malformed_request", "name the claimant in the worker_id query parameter", nil)
		return
	}
	if !h.actAs(w, r, workerID) {
		return
	}
	output, err := io.ReadAll(r.Body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	info, err := h.cfg.Work.PutStepOutput(r.Context(), r.PathValue("id"), workerID, output)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// stepOutput is GET /api/v1/steps/{id}/output: the output kept for the
// step, as plain text.
func (h handlers) stepOutput(w http.ResponseWriter, r *http.Request) {
	output, at, err := h.cfg.Work.StepOutput(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Last-Modified", at.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(output)
}

// isStepOutputUpload reports whether r uploads a step's output, the one
// request whose body may be larger than MaxBodyBytes.
func isStepOutputUpload(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/v1/steps/")
	id, tail, _ := strings.Cut(rest, "/")
	return ok && r.Method == http.MethodPut && id != "" && tail == "output"
}
