package api

import (
	"net/http"
)

// codehostToken is POST /api/v1/work/{id}/codehost-token: the worker holding the
// item asks for a short-lived code host token for one of its repositories.
// The token is in the response only; it is never logged. The commit identity
// rides along when there is one.
func (h handlers) codehostToken(w http.ResponseWriter, r *http.Request) {
	if h.cfg.MintToken == nil {
		writeError(w, r, http.StatusServiceUnavailable, "codehost_not_configured",
			"no GitHub App is configured; start factory serve with --github-app-id and --github-app-key-file", nil)
		return
	}
	var req struct {
		WorkerID string `json:"worker_id"`
		Repo     string `json:"repo"`
	}
	if err := decode(r, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	token, err := h.cfg.Work.RepoToken(r.Context(), r.PathValue("id"), req.WorkerID, req.Repo, h.cfg.MintToken)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	reply := map[string]any{"repo": req.Repo, "token": token.Token, "expires_at": token.ExpiresAt}
	if h.cfg.CommitIdentity != nil {
		if id, err := h.cfg.CommitIdentity(r.Context()); err != nil {
			h.cfg.Logger.WarnContext(r.Context(), "no commit identity for the worker; it commits as factory", "err", err)
		} else {
			reply["commit_identity"] = id
		}
	}
	writeJSON(w, http.StatusOK, reply)
}
