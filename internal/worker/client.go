package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/autobutler-org/factory/internal/apiclient"
	"github.com/autobutler-org/factory/internal/codehost"
)

// ErrLost matches an error that says the worker no longer holds the step:
// 409 not_claimant or lease_expired. The worker stops working on the step
// and does not report on it again.
var ErrLost = errors.New("the step's claim was lost")

// APIError is an error reply from the factory server.
type APIError = apiclient.Error

// Client speaks the worker side of the step protocol
// (docs/architecture/workers.md) to a factory server.
type Client struct {
	// URL is the server's base URL, such as http://factory:8080.
	URL string
	// Token returns the bearer token for one request, or "" for none. It is
	// called for every request, so a token file the kubelet refreshes is
	// read fresh.
	Token func() (string, error)
	// HTTP is the client to use; nil means http.DefaultClient.
	HTTP *http.Client
}

// Step is a claimed step, as the claim response carries it.
type Step struct {
	ID         string          `json:"id"`
	Key        string          `json:"key"`
	Kind       string          `json:"kind"`
	Params     json.RawMessage `json:"params"`
	SandboxKey string          `json:"sandbox_key"`
	Attempts   int             `json:"attempts"`
}

// Item is the part of a work item the worker reads.
type Item struct {
	ID          string   `json:"id"`
	Source      string   `json:"source"`
	ExternalURL string   `json:"external_url"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Repos       []string `json:"repos"`
}

// Output is a pull request or commit, as the claim lists the item's outputs
// and as a result records one.
type Output struct {
	Repo   string `json:"repo"`
	Kind   string `json:"kind"`
	Branch string `json:"branch"`
	URL    string `json:"url"`
	SHA    string `json:"sha"`
	Status string `json:"status"`
}

// Claim is what a claim hands the worker: the step, its item, the results
// of the graph's succeeded steps by key, and the item's outputs.
type Claim struct {
	Step    Step                       `json:"step"`
	Item    Item                       `json:"item"`
	Results map[string]json.RawMessage `json:"results"`
	Outputs []Output                   `json:"outputs"`
}

// Result is how a step ended. Outcome is succeeded, failed or blocked.
type Result struct {
	Outcome   string   `json:"outcome"`
	Result    any      `json:"result"`
	Reason    string   `json:"reason"`
	Retryable bool     `json:"retryable"`
	Outputs   []Output `json:"outputs"`
}

// call sends one JSON request and decodes a 200 reply into out. ok is false
// for a 204. An error reply that means the claim is gone also matches ErrLost.
func (c *Client) call(ctx context.Context, method, path string, in, out any) (ok bool, err error) {
	api := apiclient.Client{URL: c.URL, Token: c.Token, HTTP: c.HTTP}
	ok, err = api.Call(ctx, method, path, nil, in, out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && (apiErr.Code == "not_claimant" || apiErr.Code == "lease_expired") {
		return false, fmt.Errorf("%w: %w", ErrLost, err)
	}
	return ok, err
}

// Register registers the worker as a cluster worker named name and returns
// its ID. The same name gets the same ID back.
func (c *Client) Register(ctx context.Context, name, version string, harnesses []string) (string, error) {
	in := map[string]any{"name": name, "kind": "cluster", "version": version, "capabilities": map[string]any{"harnesses": harnesses}}
	var w struct {
		ID string `json:"id"`
	}
	if _, err := c.call(ctx, http.MethodPost, "/api/v1/workers", in, &w); err != nil {
		return "", fmt.Errorf("register: %w", err)
	}
	return w.ID, nil
}

// Heartbeat extends the worker's leases and returns the IDs it still holds
// and the lease length.
func (c *Client) Heartbeat(ctx context.Context, workerID string) (held []string, lease time.Duration, err error) {
	var hb struct {
		Held         []string `json:"held"`
		LeaseSeconds float64  `json:"lease_seconds"`
	}
	if _, err := c.call(ctx, http.MethodPost, "/api/v1/workers/"+workerID+"/heartbeat", nil, &hb); err != nil {
		return nil, 0, fmt.Errorf("heartbeat: %w", err)
	}
	return hb.Held, time.Duration(hb.LeaseSeconds * float64(time.Second)), nil
}

// Claim claims the next step, restricted to sandboxKey when it is set. ok
// is false when the server has nothing to hand out.
func (c *Client) Claim(ctx context.Context, workerID, sandboxKey string) (claim Claim, ok bool, err error) {
	var in any
	if sandboxKey != "" {
		in = map[string]string{"sandbox_key": sandboxKey}
	}
	ok, err = c.call(ctx, http.MethodPost, "/api/v1/workers/"+workerID+"/claim", in, &claim)
	if err != nil {
		return Claim{}, false, fmt.Errorf("claim: %w", err)
	}
	return claim, ok, nil
}

// Stage reports progress on a step and records outputs pushed so far.
func (c *Client) Stage(ctx context.Context, stepID, workerID, stage string, outputs []Output) error {
	in := map[string]any{"worker_id": workerID, "stage": stage, "outputs": outputs}
	if _, err := c.call(ctx, http.MethodPost, "/api/v1/steps/"+stepID+"/stage", in, nil); err != nil {
		return fmt.Errorf("report stage %s: %w", stage, err)
	}
	return nil
}

// Report reports a step's result. The claim ends.
func (c *Client) Report(ctx context.Context, stepID, workerID string, r Result) error {
	in := struct {
		WorkerID string `json:"worker_id"`
		Result
	}{workerID, r}
	if _, err := c.call(ctx, http.MethodPost, "/api/v1/steps/"+stepID+"/result", in, nil); err != nil {
		return fmt.Errorf("report the result: %w", err)
	}
	return nil
}

// Release gives a step back untouched: no attempt is counted.
func (c *Client) Release(ctx context.Context, stepID, workerID, reason string) error {
	in := map[string]string{"worker_id": workerID, "reason": reason}
	if _, err := c.call(ctx, http.MethodPost, "/api/v1/steps/"+stepID+"/release", in, nil); err != nil {
		return fmt.Errorf("release: %w", err)
	}
	return nil
}

// CodehostToken asks for a code host token for repo, which the worker's
// claim on item itemID must cover.
func (c *Client) CodehostToken(ctx context.Context, itemID, workerID, repo string) (codehost.Token, error) {
	var token codehost.Token
	in := map[string]string{"worker_id": workerID, "repo": repo}
	if _, err := c.call(ctx, http.MethodPost, "/api/v1/work/"+itemID+"/codehost-token", in, &token); err != nil {
		return codehost.Token{}, fmt.Errorf("get a code host token for %s: %w", repo, err)
	}
	return token, nil
}
