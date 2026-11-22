package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/autobutler-org/factory/internal/api"
	"github.com/autobutler-org/factory/internal/codehost"
	"github.com/autobutler-org/factory/internal/db/dbtest"
	"github.com/autobutler-org/factory/internal/sandbox"
	"github.com/autobutler-org/factory/internal/work"
)

// identityServer is a server with workload identity on: identities maps a
// projected ServiceAccount token to the sandbox the cluster says it proves.
func identityServer(t *testing.T, identities map[string]sandbox.Identity) (*client, *work.Service) {
	t.Helper()
	svc := work.New(dbtest.NewDB(t))
	srv := httptest.NewServer(api.New(api.Config{
		Work:   svc,
		Ready:  func(context.Context) error { return nil },
		Token:  token,
		Logger: slog.New(slog.DiscardHandler),
		MintToken: func(_ context.Context, repo string) (codehost.Token, error) {
			return codehost.Token{Token: "ghs_for_" + repo}, nil
		},
		IdentifyWorker: func(_ context.Context, bearer string) (sandbox.Identity, error) {
			id, ok := identities[bearer]
			if !ok {
				return sandbox.Identity{}, fmt.Errorf("%w: [invalid bearer token, service account token has expired]", sandbox.ErrUnauthenticated)
			}
			return id, nil
		},
	}))
	t.Cleanup(srv.Close)
	return &client{t: t, srv: srv}, svc
}

// plan has the launcher's part done: ready items planned and in progress.
func plan(t *testing.T, svc *work.Service) {
	t.Helper()
	if _, err := svc.StartReady(t.Context(), []string{"claude-code"}, 10); err != nil {
		t.Fatal(err)
	}
}

// as sends a request with bearer instead of the shared token.
func (c *client) as(bearer, method, path, body string, out any) *http.Response {
	c.t.Helper()
	req, err := http.NewRequest(method, c.srv.URL+path, strings.NewReader(body))
	if err != nil {
		c.t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return resp
}

type podWorker struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Credential string `json:"credential"`
}

func TestWorkerCredentialsAreBound(t *testing.T) {
	identities := map[string]sandbox.Identity{}
	c, svc := identityServer(t, identities)
	it := c.enqueue(`{"title":"x","repos":["o/a","o/b"]}`).Item
	plan(t, svc)
	identities["sa-a"] = sandbox.Identity{Sandbox: "worker-a", Key: it.ID + "/o/a", Pod: "worker-a-1", PodUID: "uid-a"}
	identities["sa-b"] = sandbox.Identity{Sandbox: "worker-b", Key: it.ID + "/o/b", Pod: "worker-b-1", PodUID: "uid-b"}

	register := func(sa string) podWorker {
		t.Helper()
		var w podWorker
		if resp := c.as(sa, http.MethodPost, "/api/v1/workers", `{"name":"whatever","kind":"cluster","capabilities":{"harnesses":["claude-code"]}}`, &w); resp.StatusCode != http.StatusOK {
			t.Fatalf("register with %s: %d", sa, resp.StatusCode)
		}
		return w
	}
	a, b := register("sa-a"), register("sa-b")
	if a.Name != "worker-a" || a.Kind != "cluster" || !strings.HasPrefix(a.Credential, work.CredentialPrefix) {
		t.Fatalf("registered %+v, want worker-a with a credential", a)
	}

	// The token file is spent once the pod has registered.
	var e apiError
	if resp := c.as("sa-a", http.MethodPost, "/api/v1/workers", `{"name":"worker-a","kind":"cluster"}`, &e); resp.StatusCode != http.StatusConflict || e.Error.Code != "already_registered" {
		t.Errorf("second registration: %d %q, want 409 already_registered", resp.StatusCode, e.Error.Code)
	}

	// Each worker claims only its own chain, whatever sandbox_key it sends.
	claim := func(w podWorker, body string) (claimJSON, int) {
		t.Helper()
		var cl claimJSON
		resp := c.as(w.Credential, http.MethodPost, "/api/v1/workers/"+w.ID+"/claim", body, &cl)
		return cl, resp.StatusCode
	}
	if _, status := claim(a, `{"sandbox_key":"`+it.ID+`/o/b"}`); status != http.StatusForbidden {
		t.Errorf("a claiming b's chain: %d, want 403", status)
	}
	clA, status := claim(a, "")
	if status != http.StatusOK || clA.Step.SandboxKey != it.ID+"/o/a" {
		t.Fatalf("a's claim: %d %+v, want a step of o/a", status, clA.Step)
	}
	clB, status := claim(b, `{"sandbox_key":"`+it.ID+`/o/b"}`)
	if status != http.StatusOK || clB.Step.SandboxKey != it.ID+"/o/b" {
		t.Fatalf("b's claim: %d %+v, want a step of o/b", status, clB.Step)
	}

	for _, tc := range []struct {
		name, method, path, body, code string
		status                         int
	}{
		{"heartbeat for a", http.MethodPost, "/api/v1/workers/" + a.ID + "/heartbeat", "", "forbidden", http.StatusForbidden},
		{"claim as a", http.MethodPost, "/api/v1/workers/" + a.ID + "/claim", "", "forbidden", http.StatusForbidden},
		{"report a's step as a", http.MethodPost, "/api/v1/steps/" + clA.Step.ID + "/result", `{"worker_id":"` + a.ID + `","outcome":"succeeded","result":{}}`, "forbidden", http.StatusForbidden},
		{"report a's step as b", http.MethodPost, "/api/v1/steps/" + clA.Step.ID + "/result", `{"worker_id":"` + b.ID + `","outcome":"succeeded","result":{}}`, "not_claimant", http.StatusConflict},
		{"stage a's step", http.MethodPost, "/api/v1/steps/" + clA.Step.ID + "/stage", `{"worker_id":"` + a.ID + `","stage":"x"}`, "forbidden", http.StatusForbidden},
		{"release a's step", http.MethodPost, "/api/v1/steps/" + clA.Step.ID + "/release", `{"worker_id":"` + a.ID + `"}`, "forbidden", http.StatusForbidden},
		{"token as a", http.MethodPost, "/api/v1/work/" + it.ID + "/codehost-token", `{"worker_id":"` + a.ID + `","repo":"o/a"}`, "forbidden", http.StatusForbidden},
		{"token for a's repository", http.MethodPost, "/api/v1/work/" + it.ID + "/codehost-token", `{"worker_id":"` + b.ID + `","repo":"o/a"}`, "repo_not_allowed", http.StatusForbidden},
		{"list work", http.MethodGet, "/api/v1/work", "", "forbidden", http.StatusForbidden},
		{"enqueue", http.MethodPost, "/api/v1/work", `{"title":"y","repos":["o/a"]}`, "forbidden", http.StatusForbidden},
		{"claim the item whole", http.MethodPost, "/api/v1/work/" + it.ID + "/claim", `{"worker_id":"` + b.ID + `"}`, "forbidden", http.StatusForbidden},
		{"transition", http.MethodPost, "/api/v1/work/" + it.ID + "/transitions", `{"to":"blocked","reason":"x"}`, "forbidden", http.StatusForbidden},
		{"register again", http.MethodPost, "/api/v1/workers", `{"name":"worker-b","kind":"cluster"}`, "forbidden", http.StatusForbidden},
	} {
		t.Run("b's credential: "+tc.name, func(t *testing.T) {
			var e apiError
			if resp := c.as(b.Credential, tc.method, tc.path, tc.body, &e); resp.StatusCode != tc.status || e.Error.Code != tc.code {
				t.Errorf("%d %q (%s), want %d %q", resp.StatusCode, e.Error.Code, e.Error.Message, tc.status, tc.code)
			}
		})
	}

	// What b may do, it may do with its own credential.
	var held struct {
		Held []string `json:"held"`
	}
	if resp := c.as(b.Credential, http.MethodPost, "/api/v1/workers/"+b.ID+"/heartbeat", "", &held); resp.StatusCode != http.StatusOK || len(held.Held) != 1 || held.Held[0] != clB.Step.ID {
		t.Errorf("b's heartbeat: %d %v, want its step", resp.StatusCode, held.Held)
	}
	var tok struct {
		Token string `json:"token"`
	}
	if resp := c.as(b.Credential, http.MethodPost, "/api/v1/work/"+it.ID+"/codehost-token", `{"worker_id":"`+b.ID+`","repo":"o/b"}`, &tok); resp.StatusCode != http.StatusOK || tok.Token != "ghs_for_o/b" {
		t.Errorf("b's token: %d %q", resp.StatusCode, tok.Token)
	}
	if resp := c.as(b.Credential, http.MethodPost, "/api/v1/steps/"+clB.Step.ID+"/stage", `{"worker_id":"`+b.ID+`","stage":"worktree_ready"}`, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("b's stage: %d", resp.StatusCode)
	}
	if resp := c.as(b.Credential, http.MethodPost, "/api/v1/steps/"+clB.Step.ID+"/result", `{"worker_id":"`+b.ID+`","outcome":"succeeded","result":{}}`, nil); resp.StatusCode != http.StatusOK {
		t.Errorf("b's result: %d", resp.StatusCode)
	}
}

// TestSharedTokenCannotActAsAWorker: with workload identity on, the token
// people use is refused wherever it would act as a worker, and still works
// elsewhere.
func TestSharedTokenCannotActAsAWorker(t *testing.T) {
	identities := map[string]sandbox.Identity{}
	c, svc := identityServer(t, identities)
	it := c.enqueue(`{"title":"x","repos":["o/a"]}`).Item
	plan(t, svc)
	identities["sa-a"] = sandbox.Identity{Sandbox: "worker-a", Key: it.ID + "/o/a", Pod: "worker-a-1", PodUID: "uid-a"}
	var a podWorker
	c.as("sa-a", http.MethodPost, "/api/v1/workers", `{"name":"worker-a","kind":"cluster"}`, &a)
	var cl claimJSON
	if resp := c.as(a.Credential, http.MethodPost, "/api/v1/workers/"+a.ID+"/claim", "", &cl); resp.StatusCode != http.StatusOK {
		t.Fatalf("claim: %d", resp.StatusCode)
	}

	for _, tc := range []struct{ name, method, path, body string }{
		{"register", http.MethodPost, "/api/v1/workers", `{"name":"x","kind":"cluster"}`},
		{"register local", http.MethodPost, "/api/v1/workers", `{"name":"x","kind":"local"}`},
		{"heartbeat", http.MethodPost, "/api/v1/workers/" + a.ID + "/heartbeat", ""},
		{"claim a step", http.MethodPost, "/api/v1/workers/" + a.ID + "/claim", ""},
		{"stage", http.MethodPost, "/api/v1/steps/" + cl.Step.ID + "/stage", `{"worker_id":"` + a.ID + `","stage":"x"}`},
		{"result", http.MethodPost, "/api/v1/steps/" + cl.Step.ID + "/result", `{"worker_id":"` + a.ID + `","outcome":"succeeded","result":{}}`},
		{"release", http.MethodPost, "/api/v1/steps/" + cl.Step.ID + "/release", `{"worker_id":"` + a.ID + `"}`},
		{"codehost token", http.MethodPost, "/api/v1/work/" + it.ID + "/codehost-token", `{"worker_id":"` + a.ID + `","repo":"o/a"}`},
		{"claim the item", http.MethodPost, "/api/v1/work/" + it.ID + "/claim", `{"worker_id":"` + a.ID + `"}`},
		{"item stage", http.MethodPost, "/api/v1/work/" + it.ID + "/stage", `{"worker_id":"` + a.ID + `","stage":"x"}`},
		{"item result", http.MethodPost, "/api/v1/work/" + it.ID + "/result", `{"worker_id":"` + a.ID + `","outcome":"succeeded"}`},
		{"item release", http.MethodPost, "/api/v1/work/" + it.ID + "/release", `{"worker_id":"` + a.ID + `"}`},
		{"worker output", http.MethodPost, "/api/v1/work/" + it.ID + "/outputs", `{"worker_id":"` + a.ID + `","repo":"o/a","kind":"commit","sha":"abc"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e apiError
			resp := c.do(tc.method, tc.path, tc.body, &e)
			if resp.StatusCode != http.StatusForbidden || e.Error.Code != "workload_identity_required" || !strings.Contains(e.Error.Message, "ServiceAccount token") {
				t.Errorf("%d %q %q, want 403 workload_identity_required saying how a worker authenticates", resp.StatusCode, e.Error.Code, e.Error.Message)
			}
		})
	}
	if resp := c.do(http.MethodGet, "/api/v1/work/"+it.ID, "", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("shared token on a person's route: %d, want 200", resp.StatusCode)
	}
}

func TestWorkerTokensRefused(t *testing.T) {
	c, _ := identityServer(t, nil)
	for _, tc := range []struct{ name, bearer, want string }{
		{"expired service account token", "eyJhbGciOiJSUzI1NiJ9.expired.sig", "has expired"},
		{"no token", "", "Bearer"},
		{"unknown credential", work.CredentialPrefix + "forged", "registers once per pod"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var e apiError
			path := "/api/v1/workers"
			if strings.HasPrefix(tc.bearer, work.CredentialPrefix) {
				path = "/api/v1/workers/x/heartbeat"
			}
			resp := c.as(tc.bearer, http.MethodPost, path, `{"name":"x","kind":"cluster"}`, &e)
			if resp.StatusCode != http.StatusUnauthorized || e.Error.Code != "unauthorized" || !strings.Contains(e.Error.Message, tc.want) {
				t.Errorf("%d %q %q, want 401 unauthorized naming %q", resp.StatusCode, e.Error.Code, e.Error.Message, tc.want)
			}
		})
	}
	var e apiError
	c.as("eyJhbGciOiJSUzI1NiJ9.expired.sig", http.MethodPost, "/api/v1/workers", `{"name":"x","kind":"cluster"}`, &e)
	if !strings.Contains(e.Error.Message, "/var/run/factory/token") {
		t.Errorf("message %q does not say what to do", e.Error.Message)
	}
}
