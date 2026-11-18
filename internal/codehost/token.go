package codehost

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultGitHubAPI is the GitHub REST API base URL.
const DefaultGitHubAPI = "https://api.github.com"

// ErrBadPrivateKey is returned for a GitHub App private key that cannot be
// used. Its message never includes the key.
var ErrBadPrivateKey = errors.New("GitHub App private key is not a PEM-encoded RSA key")

// Token is a short-lived GitHub App installation token for one repository.
type Token struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// TokenMinter mints GitHub App installation tokens.
type TokenMinter struct {
	// AppID is the GitHub App's ID (or client ID), used as the JWT issuer.
	AppID string
	// Key is the GitHub App's private key.
	Key *rsa.PrivateKey
	// API is the GitHub REST API base URL; empty means DefaultGitHubAPI.
	API string
	// Client is the HTTP client to use; nil means http.DefaultClient.
	Client *http.Client
}

// ParsePrivateKey parses a GitHub App private key in PEM form, PKCS#1 or
// PKCS#8.
func ParsePrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("%w: no PEM block found", ErrBadPrivateKey)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBadPrivateKey, err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: it is not an RSA key", ErrBadPrivateKey)
	}
	return key, nil
}

// appJWT signs the RS256 JWT that authenticates as the App itself.
func (m TokenMinter) appJWT(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	claims, err := json.Marshal(map[string]any{
		"iss": m.AppID,
		"iat": now.Add(-time.Minute).Unix(), // allow for clock drift, as GitHub advises
		"exp": now.Add(9 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.Key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign app jwt: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// appCall makes one request authenticated as the App and decodes a reply
// with status want into out.
func (m TokenMinter) appCall(ctx context.Context, jwt, method, path string, in any, want int, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	api := m.API
	if api == "" {
		api = DefaultGitHubAPI
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(api, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	client := m.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("github %s %s: %w", method, path, err)
	}
	if resp.StatusCode != want {
		return fmt.Errorf("github %s %s: %s: %s", method, path, resp.Status, data)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("github %s %s: decode: %w", method, path, err)
	}
	return nil
}

// Mint returns an installation token for repo ("owner/name") alone, limited
// to what a worker needs: push a branch, including workflow files
// (workflows: write), open, retarget, comment on and merge a pull request
// (pull_requests: write), add it to a merge queue (merge_queues: write), list
// check runs, read branch protection before a merge (administration: read),
// and read an Actions job log (actions: read). It asks for nothing on the
// tracker side: this package does not touch it. GitHub expires installation tokens
// after one hour, and each call mints a new one. It looks up the App's
// installation on repo first, so one App installed on several owners works.
func (m TokenMinter) Mint(ctx context.Context, repo string) (Token, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Token{}, fmt.Errorf("repository %q is not owner/name", repo)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	jwt, err := m.appJWT(time.Now())
	if err != nil {
		return Token{}, err
	}
	var installation struct {
		ID int64 `json:"id"`
	}
	if err := m.appCall(ctx, jwt, http.MethodGet, "/repos/"+repo+"/installation", nil, http.StatusOK, &installation); err != nil {
		return Token{}, fmt.Errorf("find the GitHub App installation on %s: %w", repo, err)
	}
	in := map[string]any{
		"repositories": []string{name},
		"permissions": map[string]string{
			"contents":       "write",
			"pull_requests":  "write",
			"checks":         "read",
			"administration": "read",
			"actions":        "read",
			"workflows":      "write",
			"merge_queues":   "write",
		},
	}
	var token Token
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installation.ID)
	if err := m.appCall(ctx, jwt, http.MethodPost, path, in, http.StatusCreated, &token); err != nil {
		return Token{}, fmt.Errorf("mint an installation token for %s: %w", repo, err)
	}
	return token, nil
}

// tokenRefreshMargin is how long before its expiry a token is replaced.
const tokenRefreshMargin = 5 * time.Minute

// RefreshingToken holds one installation token and fetches a new one
// whenever the one held is within five minutes of its expiry, so work that
// runs longer than a token lives keeps working. Its Get fits GitHub.Tokens.
// The zero value with Fetch set is ready to use.
type RefreshingToken struct {
	// Fetch asks the factory for a new token.
	Fetch func(context.Context) (Token, error)

	mu  sync.Mutex
	cur Token
}

// Get returns a token that is not about to expire. A failed refresh returns
// an error and keeps the old token, so the next call tries again.
func (r *RefreshingToken) Get(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !stale(r.cur, time.Now()) {
		return r.cur.Token, nil
	}
	token, err := r.Fetch(ctx)
	if err != nil {
		return "", fmt.Errorf("refresh the GitHub token: %w", err)
	}
	r.cur = token
	return token.Token, nil
}

// Current returns the token held now without refreshing it, or "" before the
// first Get. It is the one to revoke when the work ends.
func (r *RefreshingToken) Current() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur.Token
}

// stale reports whether token must be replaced at now: there is none yet,
// or it expires within tokenRefreshMargin. A token without an expiry is
// never stale.
func stale(token Token, now time.Time) bool {
	if token.Token == "" {
		return true
	}
	return !token.ExpiresAt.IsZero() && !now.Before(token.ExpiresAt.Add(-tokenRefreshMargin))
}
