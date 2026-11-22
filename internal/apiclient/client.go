// Package apiclient sends requests, JSON or a raw body, to a factory server's HTTP API
// (docs/api.md) and turns its error replies into *Error values. The worker's
// protocol client and the operator CLI both use it. It knows the routes'
// shapes but no state rules, and it never retries.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// requestTimeout bounds one call to the factory server.
const requestTimeout = 30 * time.Second

// ErrUnreachable matches a request that got no reply at all: the server is
// down, the address is wrong, or the network is in the way.
var ErrUnreachable = errors.New("factory server unreachable")

// Error is an error reply from the factory server.
type Error struct {
	Status    int
	Code      string
	Message   string
	RequestID string
	// CurrentState and Allowed come with illegal_transition: the item's
	// state and the states it may move to from there.
	CurrentState string
	Allowed      []string
}

func (e *Error) Error() string {
	return fmt.Sprintf("factory server: %d %s: %s", e.Status, e.Code, e.Message)
}

// Client calls one factory server.
type Client struct {
	// URL is the server's base URL, such as http://factory:8080.
	URL string
	// Token returns the bearer token for one request, or "" for none. It is
	// called for every request, so a token file the kubelet refreshes is
	// read fresh. Nil sends none.
	Token func() (string, error)
	// HTTP is the client to use; nil means http.DefaultClient.
	HTTP *http.Client
}

// Call sends one request, with in as its JSON body when it is not nil and
// the headers in header, and decodes a 200 or 201 reply into out when out is
// not nil. ok is false for a 204. Any other status is an *Error.
func (c *Client) Call(ctx context.Context, method, path string, header http.Header, in, out any) (ok bool, err error) {
	if in == nil {
		return c.Send(ctx, method, path, header, "", nil, out)
	}
	data, err := json.Marshal(in)
	if err != nil {
		return false, err
	}
	return c.Send(ctx, method, path, header, "application/json", data, out)
}

// Send is Call with a body that is not JSON: body as it is, sent as
// contentType when body is not nil.
func (c *Client) Send(ctx context.Context, method, path string, header http.Header, contentType string, body []byte, out any) (ok bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.URL, "/")+path, reader)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", method, path, err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if body != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Token != nil {
		token, err := c.Token()
		if err != nil {
			return false, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("%s %s: %w: %w. Check that the factory server is reachable at %s", method, path, ErrUnreachable, err, c.URL)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return false, fmt.Errorf("%s %s: %w", method, path, err)
	}
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return false, nil
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated:
		var e struct {
			Error struct {
				Code         string   `json:"code"`
				Message      string   `json:"message"`
				RequestID    string   `json:"request_id"`
				CurrentState string   `json:"current_state"`
				Allowed      []string `json:"allowed"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return false, fmt.Errorf("%s %s: %w", method, path, &Error{
			Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message, RequestID: e.Error.RequestID,
			CurrentState: e.Error.CurrentState, Allowed: e.Error.Allowed,
		})
	case out != nil:
		if err := json.Unmarshal(data, out); err != nil {
			return false, fmt.Errorf("%s %s: decode the reply: %w", method, path, err)
		}
	}
	return true, nil
}
