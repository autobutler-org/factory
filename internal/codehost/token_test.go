package codehost

import (
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
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeApp is a GitHub API that finds the App's installation on o/r and
// issues one installation token, after checking the App JWT against the
// App's public key.
func fakeApp(t *testing.T, key *rsa.PublicKey, expires time.Time) *httptest.Server {
	t.Helper()
	checkJWT := func(r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Fatalf("Authorization = %q, want a bearer JWT", r.Header.Get("Authorization"))
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
			t.Errorf("app JWT signature: %v", err)
		}
		var claims struct {
			Iss      string `json:"iss"`
			Iat, Exp int64
		}
		claimBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
		if err := json.Unmarshal(claimBytes, &claims); err != nil {
			t.Errorf("app JWT claims: %v", err)
		}
		if now := time.Now().Unix(); claims.Iss != "12345" || claims.Iat > now || claims.Exp <= now || claims.Exp > now+600 {
			t.Errorf("app JWT claims = %+v, want issuer 12345 and a lifetime within GitHub's 10 minute cap", claims)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/autobutler-factory[bot]" {
			// A public profile, read without auth: the App JWT is for /app endpoints only.
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("GET %s: Authorization = %q, want none", r.URL.Path, got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "autobutler-factory[bot]", "id": 342131938, "type": "Bot"})
			return
		}
		checkJWT(r)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 12345, "slug": "autobutler-factory"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/installation":
			_ = json.NewEncoder(w).Encode(map[string]int64{"id": 61234567})
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/61234567/access_tokens":
			var body struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			reqBody, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(reqBody, &body); err != nil || len(body.Repositories) != 1 || body.Repositories[0] != "r" {
				t.Errorf("token request body = %s, want it limited to the one repository", reqBody)
			}
			want := map[string]string{
				"contents":       "write",
				"pull_requests":  "write",
				"checks":         "read",
				"administration": "read",
				"actions":        "read",
				"workflows":      "write",
				"merge_queues":   "write",
			}
			if fmt.Sprint(body.Permissions) != fmt.Sprint(want) {
				t.Errorf("permissions = %v, want %v", body.Permissions, want)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Token{Token: "ghs_fake", ExpiresAt: expires})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMintIsScopedToOneRepository(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	gh := fakeApp(t, &key.PublicKey, expires)
	got, err := TokenMinter{AppID: "12345", Key: parsed, API: gh.URL}.Mint(t.Context(), "o/r")
	if err != nil || got.Token != "ghs_fake" || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("Mint = %+v, %v; want ghs_fake expiring at %s", got, err, expires)
	}
	if _, err := (TokenMinter{AppID: "12345", Key: parsed, API: gh.URL}).Mint(t.Context(), "no-slash"); err == nil {
		t.Error("Mint for a repository without an owner: no error")
	}
}

func TestMintReportsGitHubRefusal(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Integration not found"}`, http.StatusNotFound)
	}))
	t.Cleanup(gh.Close)
	_, err = TokenMinter{AppID: "1", Key: key, API: gh.URL}.Mint(t.Context(), "o/r")
	if err == nil || !strings.Contains(err.Error(), "Integration not found") {
		t.Errorf("err = %v, want GitHub's message", err)
	}
}

func TestParsePrivateKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})); err != nil {
		t.Errorf("PKCS#8 key: %v", err)
	}
	secret := "-----BEGIN RSA PRIVATE KEY-----\nc2VjcmV0LWtleS1ieXRlcw==\n-----END RSA PRIVATE KEY-----\n"
	_, err = ParsePrivateKey([]byte(secret))
	if err == nil || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Errorf("garbage key = %v, want an error that does not echo the key", err)
	}
	if _, err := ParsePrivateKey([]byte("not pem")); !errors.Is(err, ErrBadPrivateKey) {
		t.Errorf("not PEM = %v, want ErrBadPrivateKey", err)
	}
}

func TestTokenStale(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		token Token
		want  bool
	}{
		{"none yet", Token{}, true},
		{"no expiry is never stale", Token{Token: "t"}, false},
		{"well before the margin", Token{Token: "t", ExpiresAt: now.Add(time.Hour)}, false},
		{"a nanosecond before the margin", Token{Token: "t", ExpiresAt: now.Add(tokenRefreshMargin + time.Nanosecond)}, false},
		{"at the margin", Token{Token: "t", ExpiresAt: now.Add(tokenRefreshMargin)}, true},
		{"inside the margin", Token{Token: "t", ExpiresAt: now.Add(time.Minute)}, true},
		{"expired", Token{Token: "t", ExpiresAt: now.Add(-time.Minute)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stale(tc.token, now); got != tc.want {
				t.Errorf("stale(%+v) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

// TestRefreshingTokenRefreshesOnlyWhenStale fetches once for a token with an
// hour left, again for each one inside the refresh margin, and keeps the old
// token when a refresh fails.
func TestRefreshingTokenRefreshesOnlyWhenStale(t *testing.T) {
	var fetches atomic.Int32
	life := time.Hour
	var fail error
	tokens := &RefreshingToken{Fetch: func(context.Context) (Token, error) {
		if fail != nil {
			return Token{}, fail
		}
		n := fetches.Add(1)
		return Token{Token: fmt.Sprintf("t%d", n), ExpiresAt: time.Now().Add(life)}, nil
	}}
	if got := tokens.Current(); got != "" {
		t.Fatalf("Current before Get = %q, want empty", got)
	}
	for range 3 {
		if got, err := tokens.Get(t.Context()); err != nil || got != "t1" {
			t.Fatalf("Get = %q, %v, want t1", got, err)
		}
	}
	life = time.Minute
	tokens = &RefreshingToken{Fetch: tokens.Fetch}
	for want := 2; want <= 3; want++ {
		if got, err := tokens.Get(t.Context()); err != nil || got != fmt.Sprintf("t%d", want) {
			t.Fatalf("Get = %q, %v, want t%d", got, err, want)
		}
	}
	fail = errors.New("control plane down")
	if _, err := tokens.Get(t.Context()); !errors.Is(err, fail) {
		t.Errorf("Get with a failing fetch = %v, want the fetch error", err)
	}
	if got := tokens.Current(); got != "t3" {
		t.Errorf("Current after a failed refresh = %q, want t3 kept", got)
	}
}

// TestRefreshingTokenFeedsGitHub hands a fresh token to every call once the
// first one is accepted only until it expires.
func TestRefreshingTokenFeedsGitHub(t *testing.T) {
	s, g := newGitHub(t)
	s.Accept("fresh", time.Now().Add(time.Hour))
	g.Token = ""
	g.Tokens = (&RefreshingToken{Fetch: func(context.Context) (Token, error) {
		return Token{Token: "fresh", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}).Get
	if _, err := g.DefaultBranch(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// TestIdentity reads the App's slug with the App JWT and its bot user's ID
// without one, and builds the bot's noreply identity from them.
func TestIdentity(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh := fakeApp(t, &key.PublicKey, time.Now().Add(time.Hour))
	got, err := TokenMinter{AppID: "12345", Key: key, API: gh.URL}.Identity(t.Context())
	want := Identity{Name: "autobutler-factory[bot]", Email: "342131938+autobutler-factory[bot]@users.noreply.github.com"}
	if err != nil || got != want {
		t.Fatalf("Identity = %+v, %v; want %+v", got, err, want)
	}
}

func TestIdentityReportsGitHubRefusal(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(gh.Close)
	if _, err := (TokenMinter{AppID: "1", Key: key, API: gh.URL}).Identity(t.Context()); err == nil || !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("err = %v, want GitHub's message", err)
	}
}

// TestIdentityCache looks up once on success and again after a failure.
func TestIdentityCache(t *testing.T) {
	var lookups atomic.Int32
	fail := errors.New("github down")
	c := &IdentityCache{Lookup: func(context.Context) (Identity, error) {
		if lookups.Add(1) == 1 {
			return Identity{}, fail
		}
		return Identity{Name: "a[bot]", Email: "1+a[bot]@users.noreply.github.com"}, nil
	}}
	if _, err := c.Get(t.Context()); !errors.Is(err, fail) {
		t.Fatalf("first Get = %v, want the lookup error", err)
	}
	for range 3 {
		if got, err := c.Get(t.Context()); err != nil || got.Name != "a[bot]" {
			t.Fatalf("Get = %+v, %v", got, err)
		}
	}
	if n := lookups.Load(); n != 2 {
		t.Errorf("lookups = %d, want 2: one failed, one cached", n)
	}
}
