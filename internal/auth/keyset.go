package auth

import (
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// DefaultKeyRefreshInterval is the minimum time between two fetches of the
// signing keys.
const DefaultKeyRefreshInterval = 10 * time.Second

// errNoMatchingKey is returned when a signature matches none of the cached
// keys and the keys were refreshed too recently to fetch them again.
var errNoMatchingKey = errors.New("signature does not match any known key")

// throttledKeySet is an oidc.KeySet that fetches a JWKS and refetches it at
// most once per interval.
//
// go-oidc's RemoteKeySet refetches the keys whenever a signature does not
// verify. Since issuer, audience and expiry are checked before the signature
// and are not secret, anyone could otherwise make the webhook call GitHub on
// every request with a forged token, and get it rate-limited.
type throttledKeySet struct {
	url      string
	client   *http.Client
	interval time.Duration
	now      func() time.Time

	mu        sync.Mutex // held during a fetch, so concurrent misses share it
	keys      *oidc.StaticKeySet
	lastFetch time.Time // last attempt, successful or not
}

func newThrottledKeySet(url string, interval time.Duration) *throttledKeySet {
	return &throttledKeySet{
		url:      url,
		client:   &http.Client{Timeout: 10 * time.Second},
		interval: interval,
		now:      time.Now,
	}
}

// VerifySignature implements oidc.KeySet.
func (k *throttledKeySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	k.mu.Lock()
	keys := k.keys
	k.mu.Unlock()
	if keys != nil {
		if payload, err := keys.VerifySignature(ctx, jwt); err == nil {
			return payload, nil
		}
	}
	// Unknown key: maybe the issuer rotated its keys.
	keys, err := k.refresh(ctx, keys)
	if err != nil {
		return nil, err
	}
	return keys.VerifySignature(ctx, jwt)
}

// refresh fetches the keys unless they changed since the caller read stale,
// or the last attempt was less than interval ago.
func (k *throttledKeySet) refresh(ctx context.Context, stale *oidc.StaticKeySet) (*oidc.StaticKeySet, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.keys != stale {
		return k.keys, nil // another request refreshed them meanwhile
	}
	if !k.lastFetch.IsZero() && k.now().Sub(k.lastFetch) < k.interval {
		return nil, errNoMatchingKey
	}
	k.lastFetch = k.now()
	keys, err := k.fetch(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch signing keys: %w", err)
	}
	k.keys = keys
	return keys, nil
}

func (k *throttledKeySet) fetch(ctx context.Context) (*oidc.StaticKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", k.url, resp.StatusCode)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("%s: %w", k.url, err)
	}
	var pub []crypto.PublicKey
	for _, key := range set.Keys {
		if key.Use == "" || key.Use == "sig" {
			pub = append(pub, key.Key)
		}
	}
	if len(pub) == 0 {
		return nil, fmt.Errorf("%s: no signing keys", k.url)
	}
	return &oidc.StaticKeySet{PublicKeys: pub}, nil
}
