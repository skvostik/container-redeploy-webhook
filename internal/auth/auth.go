// Package auth decides whether a webhook caller may redeploy a container.
//
// Two methods exist:
//   - a global shared token (TOKEN env var); disabled when empty,
//   - GitHub Actions OIDC tokens, enabled per container by the
//     skvostik.redeploy.github.repository label.
//
// A container that has neither method available can never be redeployed.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/skvostik/container-redeploy-webhook/internal/labels"
)

const (
	// GitHubIssuer is the issuer of GitHub Actions OIDC tokens.
	GitHubIssuer = "https://token.actions.githubusercontent.com"
	// DefaultAudience is the default expected "aud" claim.
	DefaultAudience = "container-redeploy-webhook"
	// MinTokenLength is the minimum accepted length of the shared token.
	MinTokenLength = 16
)

// Claims are the GitHub Actions OIDC claims the webhook cares about.
type Claims struct {
	Repository   string `json:"repository"`
	RepositoryID string `json:"repository_id"`
	Ref          string `json:"ref"`
	Environment  string `json:"environment"`
}

// Verifier verifies a raw OIDC token and returns its claims.
type Verifier interface {
	Verify(ctx context.Context, rawToken string) (*Claims, error)
}

// Authorizer authenticates callers and checks them against container labels.
type Authorizer struct {
	tokenHash [32]byte
	hasToken  bool
	oidc      Verifier
}

// New returns an Authorizer. An empty token disables token authentication.
// A nil verifier disables OIDC authentication.
func New(token string, verifier Verifier) (*Authorizer, error) {
	a := &Authorizer{oidc: verifier}
	if token != "" {
		if len(token) < MinTokenLength {
			return nil, fmt.Errorf("TOKEN must be at least %d characters long", MinTokenLength)
		}
		a.tokenHash = sha256.Sum256([]byte(token))
		a.hasToken = true
	}
	return a, nil
}

// Principal is an authenticated caller.
type Principal struct {
	viaToken bool
	claims   *Claims
}

// String describes the principal for logs.
func (p *Principal) String() string {
	switch {
	case p == nil:
		return "anonymous"
	case p.viaToken:
		return "token"
	case p.claims != nil:
		return fmt.Sprintf("github:%s@%s", p.claims.Repository, p.claims.Ref)
	}
	return "anonymous"
}

// ErrUnauthenticated is returned when the bearer token is missing or invalid.
var ErrUnauthenticated = errors.New("invalid or missing bearer token")

// Authenticate validates the bearer token. It does not check any container.
func (a *Authorizer) Authenticate(ctx context.Context, bearer string) (*Principal, error) {
	if bearer == "" {
		return nil, ErrUnauthenticated
	}
	if a.hasToken {
		h := sha256.Sum256([]byte(bearer))
		if subtle.ConstantTimeCompare(h[:], a.tokenHash[:]) == 1 {
			return &Principal{viaToken: true}, nil
		}
	}
	if a.oidc != nil && strings.Count(bearer, ".") == 2 {
		claims, err := a.oidc.Verify(ctx, bearer)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUnauthenticated, err)
		}
		return &Principal{claims: claims}, nil
	}
	return nil, ErrUnauthenticated
}

// HasMethod reports whether a container with the given labels can be
// authorized by at least one method.
func (a *Authorizer) HasMethod(l map[string]string) bool {
	return a.hasToken || (a.oidc != nil && l[labels.GitHubRepository] != "")
}

// Allows reports whether the principal may redeploy a container with the given labels.
// On denial it returns a human-readable reason (for logs only).
func (a *Authorizer) Allows(p *Principal, l map[string]string) (bool, string) {
	if !a.HasMethod(l) {
		return false, "container has no authentication method configured"
	}
	if p == nil {
		return false, "not authenticated"
	}
	if p.viaToken {
		return a.hasToken, "token auth disabled"
	}
	if p.claims == nil || a.oidc == nil {
		return false, "not authenticated"
	}
	want := strings.TrimSpace(l[labels.GitHubRepository])
	if want == "" {
		return false, "container does not allow GitHub OIDC"
	}
	if !repositoryMatches(want, p.claims) {
		return false, fmt.Sprintf("repository %q (id %s) does not match label %q", p.claims.Repository, p.claims.RepositoryID, want)
	}
	if ref := l[labels.GitHubRef]; ref != "" && ref != p.claims.Ref {
		return false, fmt.Sprintf("ref %q does not match label %q", p.claims.Ref, ref)
	}
	if env := l[labels.GitHubEnvironment]; env != "" && env != p.claims.Environment {
		return false, fmt.Sprintf("environment %q does not match label %q", p.claims.Environment, env)
	}
	return true, ""
}

// repositoryMatches compares the label value with the token claims. A numeric
// value is matched against repository_id, anything else against the
// case-insensitive "owner/repo" name.
func repositoryMatches(want string, c *Claims) bool {
	if _, err := strconv.ParseUint(want, 10, 64); err == nil {
		return c.RepositoryID != "" && want == c.RepositoryID
	}
	return c.Repository != "" && strings.EqualFold(want, c.Repository)
}

// GitHubVerifier verifies GitHub Actions OIDC tokens. Signing keys are fetched
// lazily from GitHub, cached, and refetched at most every
// DefaultKeyRefreshInterval.
type GitHubVerifier struct {
	v *oidc.IDTokenVerifier
}

// NewGitHubVerifier returns a verifier for the given audience.
func NewGitHubVerifier(audience string) *GitHubVerifier {
	keys := newThrottledKeySet(GitHubIssuer+"/.well-known/jwks", DefaultKeyRefreshInterval)
	return NewVerifier(GitHubIssuer, audience, keys)
}

// NewVerifier returns a verifier with a custom issuer and key set (used in tests).
func NewVerifier(issuer, audience string, keys oidc.KeySet) *GitHubVerifier {
	return &GitHubVerifier{v: oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience})}
}

// Verify implements Verifier.
func (g *GitHubVerifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	tok, err := g.v.Verify(ctx, raw)
	if err != nil {
		return nil, err
	}
	var c Claims
	if err := tok.Claims(&c); err != nil {
		return nil, err
	}
	return &c, nil
}
