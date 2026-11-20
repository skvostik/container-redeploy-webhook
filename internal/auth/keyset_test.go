package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// jwksServer serves the public part of *key and counts the requests.
type jwksServer struct {
	*httptest.Server
	mu    sync.Mutex
	key   *rsa.PrivateKey
	fetch atomic.Int32
}

func newJWKSServer(t *testing.T, key *rsa.PrivateKey) *jwksServer {
	s := &jwksServer{key: key}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.fetch.Add(1)
		s.mu.Lock()
		pub := s.key.Public()
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: pub, KeyID: "k", Algorithm: "RS256", Use: "sig"},
		}})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) rotate(key *rsa.PrivateKey) {
	s.mu.Lock()
	s.key = key
	s.mu.Unlock()
}

func TestThrottledKeySet(t *testing.T) {
	ctx := context.Background()
	srv := newJWKSServer(t, testKey)
	now := time.Unix(1_000_000, 0)
	ks := newThrottledKeySet(srv.URL, time.Minute)
	ks.now = func() time.Time { return now }
	a, err := New("", NewVerifier(testIssuer, testAudience, ks))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := a.Authenticate(ctx, sign(t, testKey, ghClaims(nil))); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if n := srv.fetch.Load(); n != 1 {
		t.Fatalf("keys fetched %d times, want 1", n)
	}

	// Forged tokens (valid claims, wrong key) must not cause a fetch each.
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	for range 20 {
		if _, err := a.Authenticate(ctx, sign(t, other, ghClaims(nil))); err == nil {
			t.Fatal("forged token accepted")
		}
	}
	if n := srv.fetch.Load(); n != 2 {
		t.Fatalf("keys fetched %d times after forged tokens, want 2", n)
	}

	// Valid tokens keep working from the cache.
	if _, err := a.Authenticate(ctx, sign(t, testKey, ghClaims(nil))); err != nil {
		t.Fatalf("valid token rejected after forged ones: %v", err)
	}
	if n := srv.fetch.Load(); n != 2 {
		t.Fatalf("valid cached token triggered a fetch (%d)", n)
	}

	// Key rotation is picked up once the interval has passed.
	srv.rotate(other)
	now = now.Add(2 * time.Minute)
	if _, err := a.Authenticate(ctx, sign(t, other, ghClaims(nil))); err != nil {
		t.Fatalf("token signed with rotated key rejected: %v", err)
	}
}

func TestThrottledKeySetFetchError(t *testing.T) {
	ks := newThrottledKeySet("http://127.0.0.1:1/jwks", time.Minute)
	if _, err := ks.VerifySignature(context.Background(), sign(t, testKey, ghClaims(nil))); err == nil {
		t.Fatal("expected an error when the keys cannot be fetched")
	}
}
