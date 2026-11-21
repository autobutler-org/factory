package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/autobutler-org/factory/internal/work"
)

// defaultLogTail is how many lines a log read without a cursor returns.
const defaultLogTail = 500

// logReadTimeout bounds one read of a pod log, well inside the server's
// 60 second write timeout.
const logReadTimeout = 20 * time.Second

type sandboxView struct {
	Name    string        `json:"name"`
	Key     string        `json:"key"`
	Repo    string        `json:"repo"`
	Image   string        `json:"image"`
	Phase   sandbox.Phase `json:"phase"`
	Created time.Time     `json:"created"`
}

func viewOf(sb sandbox.Sandbox) sandboxView {
	_, repo, _ := strings.Cut(sb.Key, "/")
	return sandboxView{Name: sb.Name, Key: sb.Key, Repo: repo, Image: sb.Image, Phase: sb.Phase, Created: sb.Created}
}

// itemSandbox reports whether sb serves one of the item's chains: its key
// is "<item id>/<owner>/<repo>".
func itemSandbox(sb sandbox.Sandbox, itemID string) bool {
	return strings.HasPrefix(sb.Key, itemID+"/")
}

func (h handlers) sandboxesOn(w http.ResponseWriter, r *http.Request) bool {
	if h.cfg.Sandboxes == nil {
		writeError(w, r, http.StatusServiceUnavailable, "sandboxes_not_configured",
			"the launcher is off, so this server runs no sandboxes; start factory serve with --launcher", nil)
		return false
	}
	return true
}

// listSandboxes is GET /api/v1/work/{id}/sandboxes: the item's sandboxes
// that still exist, live or ended, oldest first.
func (h handlers) listSandboxes(w http.ResponseWriter, r *http.Request) {
	if !h.sandboxesOn(w, r) {
		return
	}
	all, err := h.cfg.Sandboxes.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := []sandboxView{}
	for _, sb := range all {
		if itemSandbox(sb, r.PathValue("id")) {
			out = append(out, viewOf(sb))
		}
	}
	slices.SortStableFunc(out, func(a, b sandboxView) int { return a.Created.Compare(b.Created) })
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
}

// sandboxLogs is GET /api/v1/work/{id}/sandboxes/{name}/logs: the last
// lines of a sandbox's worker log, or with ?after=<cursor> the lines since
// an earlier read.
func (h handlers) sandboxLogs(w http.ResponseWriter, r *http.Request) {
	if !h.sandboxesOn(w, r) {
		return
	}
	q, err := logQuery(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), logReadTimeout)
	defer cancel()
	name := r.PathValue("name")
	sb, err := h.cfg.Sandboxes.Status(ctx, name)
	if err == nil && !itemSandbox(sb, r.PathValue("id")) {
		err = sandbox.ErrNotFound
	}
	var logs sandbox.Logs
	if err == nil {
		logs, err = h.cfg.Sandboxes.Logs(ctx, name, q)
	}
	var notStarted *sandbox.NotStartedError
	switch {
	case errors.Is(err, sandbox.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "sandbox_not_found", fmt.Sprintf(
			"work item %s has no sandbox %s: it has ended and been removed, and its log with it (the launcher "+
				"removes a sandbox once its chain has nothing to run). GET /api/v1/work/%s/sandboxes lists the ones left",
			r.PathValue("id"), name, r.PathValue("id")), nil)
	case errors.As(err, &notStarted):
		writeJSON(w, http.StatusOK, map[string]any{
			"sandbox": viewOf(sb), "lines": []sandbox.LogLine{}, "cursor": "", "more": false,
			"pending": map[string]string{"reason": notStarted.Reason, "message": notStarted.Message},
		})
	case err != nil:
		h.fail(w, r, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"sandbox": viewOf(sb), "lines": logs.Lines, "cursor": logs.Next.String(), "more": logs.More, "pending": nil,
		})
	}
}

func logQuery(r *http.Request) (sandbox.LogQuery, error) {
	v := r.URL.Query()
	q := sandbox.LogQuery{Tail: defaultLogTail}
	if v.Has("after") {
		if v.Has("tail") {
			return q, fmt.Errorf("%w: send tail or after, not both", work.ErrInvalid)
		}
		c, err := sandbox.ParseCursor(v.Get("after"))
		if err != nil {
			return q, fmt.Errorf("%w: after: %w; send the cursor of the last response, or tail to start over", work.ErrInvalid, err)
		}
		q.After = &c
		return q, nil
	}
	if raw := v.Get("tail"); v.Has("tail") {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > sandbox.MaxLogLines {
			return q, fmt.Errorf("%w: tail must be a whole number from 1 to %d", work.ErrInvalid, sandbox.MaxLogLines)
		}
		q.Tail = n
	}
	return q, nil
}
