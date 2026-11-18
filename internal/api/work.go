package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/autobutler-org/factory/internal/work"
)

type enqueueRequest struct {
	Source      string   `json:"source"`
	ExternalID  string   `json:"external_id"`
	ExternalURL string   `json:"external_url"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Repos       []string `json:"repos"`
	State       string   `json:"state"`
	Priority    int32    `json:"priority"`
	BlockedBy   []string `json:"blocked_by"`
	Requires    []string `json:"requires"`
}

// enqueue is POST /api/v1/work, the by-value form of #18: 201 for a new
// item, 200 for an existing one.
func (h handlers) enqueue(w http.ResponseWriter, r *http.Request) {
	var req enqueueRequest
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.cfg.Work.Upsert(r.Context(), work.NewItem{
		Source: req.Source, ExternalID: req.ExternalID, ExternalURL: req.ExternalURL,
		IdempotencyKey: r.Header.Get("Idempotency-Key"),
		Title:          req.Title, Body: req.Body, Repos: req.Repos, State: req.State, Priority: req.Priority,
		BlockedBy: req.BlockedBy, Requires: req.Requires,
	}, apiActor())
	if err != nil {
		// An unknown blocked_by is a problem with the request, not a missing route.
		if errors.Is(err, work.ErrNotFound) {
			err = fmt.Errorf("%w: blocked_by: %w", work.ErrInvalid, err)
		}
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
		w.Header().Set("Location", "/api/v1/work/"+res.Item.ID)
	}
	writeJSON(w, status, map[string]any{"item": res.Item, "created": res.Created, "changed": res.Changed, "reason": res.Reason})
}

func (h handlers) listWork(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := work.Filter{State: q.Get("state"), Source: q.Get("source"), Repo: q.Get("repo"), ClaimedBy: q.Get("claimed_by"), Cursor: q.Get("cursor")}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			h.fail(w, r, fmt.Errorf("%w: limit must be a positive integer", work.ErrInvalid))
			return
		}
		f.Limit = n
	}
	items, next, err := h.cfg.Work.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}

func (h handlers) getWork(w http.ResponseWriter, r *http.Request) {
	item, err := h.cfg.Work.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h handlers) patchWork(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title    *string `json:"title"`
		Body     *string `json:"body"`
		Priority *int32  `json:"priority"`
	}
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	item, err := h.cfg.Work.Patch(r.Context(), r.PathValue("id"), work.Patch{Title: req.Title, Body: req.Body, Priority: req.Priority}, apiActor())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h handlers) transition(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To     string `json:"to"`
		Reason string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	item, err := h.cfg.Work.Transition(r.Context(), r.PathValue("id"), req.To, req.Reason, apiActor())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (h handlers) events(w http.ResponseWriter, r *http.Request) {
	events, err := h.cfg.Work.Events(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (h handlers) outputs(w http.ResponseWriter, r *http.Request) {
	outputs, err := h.cfg.Work.Outputs(r.Context(), r.PathValue("id"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outputs": outputs})
}

func (h handlers) putDependency(w http.ResponseWriter, r *http.Request) {
	if err := h.cfg.Work.AddDependency(r.Context(), r.PathValue("id"), r.PathValue("blocked_by_id"), apiActor()); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h handlers) deleteDependency(w http.ResponseWriter, r *http.Request) {
	if err := h.cfg.Work.RemoveDependency(r.Context(), r.PathValue("id"), r.PathValue("blocked_by_id"), apiActor()); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
