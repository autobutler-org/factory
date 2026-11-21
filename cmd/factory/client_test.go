package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The fixtures a fake server answers with, in the API's shapes.
const (
	itemJSON = `{"id":"0b8c1f9e-0000-4000-8000-000000000001","source":"github","external_id":"autobutler-org/quark#123",
"external_url":"https://github.com/autobutler-org/quark/issues/123","title":"add null check to the importer",
"body":"The importer crashes on a missing field.","repos":["autobutler-org/quark"],"state":"in_progress",
"stage":"implementing","state_reason":"","priority":2,"attempts":1,"outputs_done":false,
"created_at":"2026-11-18T12:00:00Z","updated_at":"2026-11-18T12:05:00Z","state_changed_at":"2026-11-18T12:01:00Z",
"idempotency_key":null,"claimed_by":null,"claimed_at":null,"lease_expires_at":null,"stage_reported_at":null,
"available_at":"2026-11-18T12:00:00Z","requires":["claude"]}`
	readyJSON = `{"id":"0b8c1f9e-0000-4000-8000-000000000002","source":null,"external_id":null,"external_url":null,
"title":"rotate the staging certificates","body":"","repos":["autobutler-org/infra"],"state":"ready","stage":"",
"state_reason":"","priority":0,"attempts":0,"outputs_done":false,"created_at":"2026-11-18T11:00:00Z",
"updated_at":"2026-11-18T11:00:00Z","state_changed_at":"2026-11-18T11:00:00Z","idempotency_key":"nightly",
"claimed_by":null,"claimed_at":null,"lease_expires_at":null,"stage_reported_at":null,
"available_at":"2026-11-18T11:00:00Z","requires":[]}`
	failedJSON = `{"id":"0b8c1f9e-0000-4000-8000-000000000003","source":null,"external_id":null,"external_url":null,
"title":"bump the linter","body":"","repos":["autobutler-org/factory"],"state":"failed","stage":"",
"state_reason":"gate failed three times","priority":0,"attempts":3,"outputs_done":false,
"created_at":"2026-11-17T09:00:00Z","updated_at":"2026-11-17T10:00:00Z","state_changed_at":"2026-11-17T10:00:00Z",
"idempotency_key":null,"claimed_by":null,"claimed_at":null,"lease_expires_at":null,"stage_reported_at":null,
"available_at":"2026-11-17T09:00:00Z","requires":[]}`
	graphJSON = `{"graph":{"id":"g1","work_item_id":"0b8c1f9e-0000-4000-8000-000000000001","version":1,"template":"default",
"inputs_hash":"h","state":"running","created_at":"2026-11-18T12:01:00Z"},
"steps":[{"id":"s1","graph_id":"g1","position":0,"key":"autobutler-org/quark/workspace","kind":"workspace","params":{},
"sandbox_key":"k","condition":"","state":"succeeded","attempts":1,"claimed_by":null,"lease_expires_at":null,"stage":"",
"stage_reported_at":null,"available_at":"2026-11-18T12:01:00Z","result":null,"started_at":null,"finished_at":null},
{"id":"s2","graph_id":"g1","position":1,"key":"autobutler-org/quark/implement","kind":"implement","params":{},
"sandbox_key":"k","condition":"","state":"running","attempts":1,"claimed_by":"w1","lease_expires_at":null,"stage":"",
"stage_reported_at":null,"available_at":"2026-11-18T12:01:00Z","result":null,"started_at":null,"finished_at":null}],
"edges":[{"step_id":"s2","depends_on_id":"s1"}]}`
	eventsJSON = `{"events":[
{"id":1,"work_item_id":"0b8c1f9e-0000-4000-8000-000000000001","at":"2026-11-18T12:00:00Z","actor_kind":"adapter","actor_id":"github","type":"created","from_state":null,"to_state":"ready","payload":{}},
{"id":2,"work_item_id":"0b8c1f9e-0000-4000-8000-000000000001","at":"2026-11-18T12:01:00Z","actor_kind":"system","actor_id":"launcher","type":"transition","from_state":"ready","to_state":"in_progress","payload":{"reason":"planned"}}]}`
	workersJSON = `{"workers":[
{"id":"w1","name":"worker-a","kind":"cluster","capabilities":{"harnesses":["claude-code"]},"version":"v0.3.0",
"registered_at":"2026-11-18T12:01:00Z","last_seen_at":"2026-11-18T12:05:00Z","state":"active",
"holds":[{"work_item_id":"0b8c1f9e-0000-4000-8000-000000000001","title":"add null check to the importer","step_id":"s2","step_key":"autobutler-org/quark/implement"}]},
{"id":"w2","name":"worker-b","kind":"cluster","capabilities":{},"version":"v0.3.0",
"registered_at":"2026-11-17T09:00:00Z","last_seen_at":"2026-11-17T10:00:00Z","state":"gone","holds":[]}]}`
	statusJSON = `{"counts":{"design":0,"ready":1,"in_progress":1,"in_review":0,"blocked":0,"failed":1,"completed":4,"wont_fix":0},
"workers":{"active":1,"gone":1},"oldest_ready":` + readyJSON + `,"recent_failures":[` + failedJSON + `]}`
)

// fakeServer answers "METHOD /path?query" with a status and a JSON body, and
// sets FACTORY_SERVER to it. Any other request fails the test.
func fakeServer(t *testing.T, routes map[string]reply) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.RequestURI()
		rep, ok := routes[key]
		if !ok {
			t.Errorf("unexpected request %s", key)
			http.Error(w, "unexpected", http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(serverEnv, srv.URL)
}

type reply struct {
	status int
	body   string
}

func ok(body string) reply { return reply{http.StatusOK, body} }

func apiError(status int, code, message string) reply {
	return reply{status, `{"error":{"code":"` + code + `","message":"` + message + `","request_id":"req-1"}}`}
}

// TestReadCommands pins each read command's table and JSON output, and its
// error paths, in golden files.
func TestReadCommands(t *testing.T) {
	const id = "0b8c1f9e-0000-4000-8000-000000000001"
	routes := map[string]reply{
		"GET /api/v1/work":                                             ok(`{"items":[` + itemJSON + `,` + readyJSON + `],"next_cursor":""}`),
		"GET /api/v1/work?limit=1&state=in_progress":                   ok(`{"items":[` + itemJSON + `],"next_cursor":"abc"}`),
		"GET /api/v1/work?state=design":                                ok(`{"items":[],"next_cursor":""}`),
		"GET /api/v1/work?state=sideways":                              apiError(422, "invalid_request", "invalid request: unknown state \"sideways\""),
		"GET /api/v1/work/" + id:                                       ok(itemJSON),
		"GET /api/v1/work/" + id + "/graph":                            ok(graphJSON),
		"GET /api/v1/work/" + id + "/events":                           ok(eventsJSON),
		"GET /api/v1/work/0b8c1f9e-0000-4000-8000-000000000002":        ok(readyJSON),
		"GET /api/v1/work/0b8c1f9e-0000-4000-8000-000000000002/graph":  apiError(404, "not_found", "not found"),
		"GET /api/v1/work/0b8c1f9e-0000-4000-8000-000000000002/events": ok(`{"events":[]}`),
		"GET /api/v1/work/nope":                                        apiError(404, "not_found", "work item not found"),
		"GET /api/v1/workers":                                          ok(workersJSON),
		"GET /api/v1/status":                                           ok(statusJSON),
	}
	cases := map[string][]string{
		"work_list":            {"work", "list"},
		"work_list_json":       {"work", "list", "--output", "json"},
		"work_list_page":       {"work", "list", "--state", "in_progress", "--limit", "1"},
		"work_list_empty":      {"work", "list", "--state", "design"},
		"work_list_bad_state":  {"work", "list", "--state", "sideways"},
		"work_list_bad_output": {"work", "list", "--output", "yaml"},
		"work_show":            {"work", "show", id},
		"work_show_json":       {"work", "show", "--output", "json", id},
		"work_show_unplanned":  {"work", "show", "0b8c1f9e-0000-4000-8000-000000000002"},
		"work_show_not_found":  {"work", "show", "nope"},
		"work_show_no_id":      {"work", "show"},
		"workers_list":         {"workers", "list"},
		"workers_list_json":    {"workers", "list", "--output", "json"},
		"status":               {"status"},
		"status_json":          {"status", "--output", "json"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			fakeServer(t, routes)
			golden(t, "read/"+name, transcript(t.Context(), args...))
		})
	}
}

// TestClientErrors pins the errors every API command shares: no server, a
// bad server URL, an unreachable server, and a missing or wrong token.
func TestClientErrors(t *testing.T) {
	t.Run("no_server", func(t *testing.T) {
		t.Setenv(serverEnv, "")
		golden(t, "client/no_server", transcript(t.Context(), "status"))
	})
	t.Run("bad_server", func(t *testing.T) {
		golden(t, "client/bad_server", transcript(t.Context(), "status", "--server", "127.0.0.1:8080"))
	})
	t.Run("unreachable", func(t *testing.T) {
		golden(t, "client/unreachable", transcript(t.Context(), "status", "--server", "http://127.0.0.1:1"))
	})
	unauthorized := map[string]reply{"GET /api/v1/status": apiError(401, "unauthorized", "send the API token")}
	t.Run("no_token", func(t *testing.T) {
		t.Setenv(apiTokenEnv, "")
		fakeServer(t, unauthorized)
		golden(t, "client/no_token", transcript(t.Context(), "status"))
	})
	t.Run("wrong_token", func(t *testing.T) {
		t.Setenv(apiTokenEnv, "wrong")
		fakeServer(t, unauthorized)
		golden(t, "client/wrong_token", transcript(t.Context(), "status"))
	})
	t.Run("server_failed", func(t *testing.T) {
		fakeServer(t, map[string]reply{"GET /api/v1/status": apiError(500, "internal", "internal error; the server log has the details")})
		golden(t, "client/server_failed", transcript(t.Context(), "status"))
	})
	t.Run("not_a_factory", func(t *testing.T) {
		fakeServer(t, map[string]reply{"GET /api/v1/status": {http.StatusBadGateway, "<html>bad gateway</html>"}})
		golden(t, "client/not_a_factory", transcript(t.Context(), "status"))
	})
	t.Run("refused", func(t *testing.T) {
		fakeServer(t, map[string]reply{"GET /api/v1/status": apiError(503, "not_ready", "database unreachable")})
		golden(t, "client/refused", transcript(t.Context(), "status"))
	})
	t.Run("older_server", func(t *testing.T) {
		fakeServer(t, map[string]reply{"GET /api/v1/status": apiError(404, "not_found", "no route for GET /api/v1/status")})
		golden(t, "client/older_server", transcript(t.Context(), "status"))
	})
}
