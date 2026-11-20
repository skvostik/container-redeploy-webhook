package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/skvostik/container-redeploy-webhook/internal/labels"
)

const (
	testIssuer   = "https://issuer.test"
	testAudience = "container-redeploy-webhook"
	goodToken    = "0123456789abcdef-secret"
)

var testKey = func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}()

func sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(map[string]string{"alg": "RS256", "typ": "JWT"})
	body := enc(claims)
	sum := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func ghClaims(over map[string]any) map[string]any {
	c := map[string]any{
		"iss":           testIssuer,
		"aud":           testAudience,
		"exp":           time.Now().Add(5 * time.Minute).Unix(),
		"iat":           time.Now().Unix(),
		"repository":    "Skvostik/App",
		"repository_id": "123456789",
		"ref":           "refs/heads/main",
		"environment":   "production",
	}
	for k, v := range over {
		c[k] = v
	}
	return c
}

func newAuthorizer(t *testing.T, token string) *Authorizer {
	t.Helper()
	v := NewVerifier(testIssuer, testAudience, &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{testKey.Public()}})
	a, err := New(token, v)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewRejectsShortToken(t *testing.T) {
	if _, err := New("short", nil); err == nil {
		t.Fatal("expected error for short token")
	}
}

func TestTokenAuth(t *testing.T) {
	ctx := context.Background()
	a := newAuthorizer(t, goodToken)
	l := map[string]string{labels.ID: "app"}

	p, err := a.Authenticate(ctx, goodToken)
	if err != nil {
		t.Fatalf("good token rejected: %v", err)
	}
	if ok, why := a.Allows(p, l); !ok {
		t.Fatalf("good token not allowed: %s", why)
	}
	for _, bad := range []string{"", "wrong-token-wrong-token", goodToken + "x"} {
		if _, err := a.Authenticate(ctx, bad); !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("token %q: expected ErrUnauthenticated, got %v", bad, err)
		}
	}
}

func TestTokenDisabledWhenUnset(t *testing.T) {
	a := newAuthorizer(t, "")
	if _, err := a.Authenticate(context.Background(), ""); err == nil {
		t.Fatal("empty bearer must never authenticate")
	}
	if a.HasMethod(map[string]string{labels.ID: "app"}) {
		t.Fatal("container without github label must have no auth method when TOKEN is unset")
	}
	if ok, _ := a.Allows(&Principal{viaToken: true}, map[string]string{labels.ID: "app"}); ok {
		t.Fatal("token principal must not be allowed when token auth is disabled")
	}
}

func TestOIDC(t *testing.T) {
	ctx := context.Background()
	a := newAuthorizer(t, "")

	tests := []struct {
		name   string
		claims map[string]any
		labels map[string]string
		authOK bool
		allow  bool
	}{
		{"repo name, case-insensitive", nil, map[string]string{labels.GitHubRepository: "skvostik/app"}, true, true},
		{"repo id", nil, map[string]string{labels.GitHubRepository: "123456789"}, true, true},
		{"wrong repo name", nil, map[string]string{labels.GitHubRepository: "skvostik/other"}, true, false},
		{"wrong repo id", nil, map[string]string{labels.GitHubRepository: "987654321"}, true, false},
		{"id label does not match name", map[string]any{"repository_id": ""}, map[string]string{labels.GitHubRepository: "123456789"}, true, false},
		{"ref matches", nil, map[string]string{labels.GitHubRepository: "skvostik/app", labels.GitHubRef: "refs/heads/main"}, true, true},
		{"ref mismatch", nil, map[string]string{labels.GitHubRepository: "skvostik/app", labels.GitHubRef: "refs/heads/prod"}, true, false},
		{"environment mismatch", nil, map[string]string{labels.GitHubRepository: "skvostik/app", labels.GitHubEnvironment: "staging"}, true, false},
		{"no github label", nil, map[string]string{}, true, false},
		{"wrong audience", map[string]any{"aud": "other"}, map[string]string{labels.GitHubRepository: "skvostik/app"}, false, false},
		{"wrong issuer", map[string]any{"iss": "https://evil.test"}, map[string]string{labels.GitHubRepository: "skvostik/app"}, false, false},
		{"expired", map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}, map[string]string{labels.GitHubRepository: "skvostik/app"}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := sign(t, testKey, ghClaims(tt.claims))
			p, err := a.Authenticate(ctx, raw)
			if (err == nil) != tt.authOK {
				t.Fatalf("authenticate: err=%v, want ok=%v", err, tt.authOK)
			}
			if ok, why := a.Allows(p, tt.labels); ok != tt.allow {
				t.Fatalf("allows=%v (%s), want %v", ok, why, tt.allow)
			}
		})
	}
}

func TestOIDCWrongKey(t *testing.T) {
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	a := newAuthorizer(t, "")
	if _, err := a.Authenticate(context.Background(), sign(t, other, ghClaims(nil))); err == nil {
		t.Fatal("token signed by an unknown key must be rejected")
	}
}

func TestTokenDoesNotGrantOIDCOnlyWhenDisabled(t *testing.T) {
	// With TOKEN set, any labelled container accepts the token.
	a := newAuthorizer(t, goodToken)
	p, _ := a.Authenticate(context.Background(), goodToken)
	if ok, _ := a.Allows(p, map[string]string{labels.GitHubRepository: "skvostik/app"}); !ok {
		t.Fatal("global token must work for containers with a github label too")
	}
	// An OIDC principal cannot use a container that has no github label even if TOKEN is set.
	po, err := a.Authenticate(context.Background(), sign(t, testKey, ghClaims(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := a.Allows(po, map[string]string{}); ok {
		t.Fatal("OIDC principal must not be allowed without github label")
	}
}
