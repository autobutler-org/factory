package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/autobutler-org/factory/internal/db"
	"github.com/autobutler-org/factory/internal/work"
)

// WorkPage is one page of GET /api/v1/work.
type WorkPage struct {
	Items      []db.WorkItem `json:"items"`
	NextCursor string        `json:"next_cursor"`
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	_, err := c.Call(ctx, http.MethodGet, path, nil, nil, out)
	return err
}

// ListWork lists work items. filter holds the query parameters the route
// takes: state, source, repo, claimed_by, limit and cursor.
func (c *Client) ListWork(ctx context.Context, filter url.Values) (WorkPage, error) {
	var page WorkPage
	path := "/api/v1/work"
	if len(filter) > 0 {
		path += "?" + filter.Encode()
	}
	err := c.get(ctx, path, &page)
	return page, err
}

// Work returns one work item.
func (c *Client) Work(ctx context.Context, id string) (db.WorkItem, error) {
	var item db.WorkItem
	err := c.get(ctx, "/api/v1/work/"+url.PathEscape(id), &item)
	return item, err
}

// Events returns an item's events, oldest first.
func (c *Client) Events(ctx context.Context, id string) ([]db.WorkEvent, error) {
	var out struct {
		Events []db.WorkEvent `json:"events"`
	}
	err := c.get(ctx, "/api/v1/work/"+url.PathEscape(id)+"/events", &out)
	return out.Events, err
}

// Graph returns an item's latest step graph, or nil before it is planned.
func (c *Client) Graph(ctx context.Context, id string) (*work.GraphView, error) {
	var g work.GraphView
	err := c.get(ctx, "/api/v1/work/"+url.PathEscape(id)+"/graph", &g)
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// Workers lists the worker registry by name.
func (c *Client) Workers(ctx context.Context) ([]work.WorkerView, error) {
	var out struct {
		Workers []work.WorkerView `json:"workers"`
	}
	err := c.get(ctx, "/api/v1/workers", &out)
	return out.Workers, err
}

// Status returns the factory at a glance.
func (c *Client) Status(ctx context.Context) (work.Status, error) {
	var st work.Status
	err := c.get(ctx, "/api/v1/status", &st)
	return st, err
}

// Enqueued is the reply to an enqueue.
type Enqueued struct {
	Item    db.WorkItem `json:"item"`
	Created bool        `json:"created"`
	Changed bool        `json:"changed"`
	Reason  string      `json:"reason"`
}

// Enqueue enqueues work by value: item is the body POST /api/v1/work takes.
// idempotencyKey, when set, makes a repeat return the first item.
func (c *Client) Enqueue(ctx context.Context, item any, idempotencyKey string) (Enqueued, error) {
	var header http.Header
	if idempotencyKey != "" {
		header = http.Header{"Idempotency-Key": {idempotencyKey}}
	}
	var out Enqueued
	_, err := c.Call(ctx, http.MethodPost, "/api/v1/work", header, item, &out)
	return out, err
}

// Transition asks to move an item to state to. A refusal is an *Error;
// illegal_transition carries the states it may move to.
func (c *Client) Transition(ctx context.Context, id, to, reason string) (db.WorkItem, error) {
	var item db.WorkItem
	in := map[string]string{"to": to, "reason": reason}
	_, err := c.Call(ctx, http.MethodPost, "/api/v1/work/"+url.PathEscape(id)+"/transitions", nil, in, &item)
	return item, err
}
