// Package registryauth reads registry credentials from a Docker config.json
// so private images can be pulled.
//
// Only static credentials stored in the "auths" section are supported
// (what `docker login` writes when no credential helper is configured).
package registryauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
)

type entry struct {
	Auth          string `json:"auth"`
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
}

// Store holds credentials keyed by registry hostname.
type Store struct {
	auths map[string]registry.AuthConfig
}

// Load reads <dir>/config.json. A missing file yields an empty store.
func Load(dir string) (*Store, error) {
	s := &Store{auths: map[string]registry.AuthConfig{}}
	if dir == "" {
		return s, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Auths map[string]entry `json:"auths"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse docker config: %w", err)
	}
	for key, e := range cfg.Auths {
		ac := registry.AuthConfig{
			Username:      e.Username,
			Password:      e.Password,
			IdentityToken: e.IdentityToken,
			ServerAddress: key,
		}
		if e.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(e.Auth)
			if err != nil {
				return nil, fmt.Errorf("decode auth for %s: %w", key, err)
			}
			user, pass, ok := strings.Cut(string(raw), ":")
			if !ok {
				return nil, fmt.Errorf("invalid auth for %s", key)
			}
			ac.Username, ac.Password = user, pass
		}
		s.auths[normalizeHost(key)] = ac
	}
	return s, nil
}

// Len returns the number of registries with credentials.
func (s *Store) Len() int { return len(s.auths) }

// For returns the encoded X-Registry-Auth value for an image reference,
// or "" when no credentials are known for its registry.
func (s *Store) For(image string) (string, error) {
	if s == nil || len(s.auths) == 0 {
		return "", nil
	}
	ref, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", err
	}
	ac, ok := s.auths[normalizeHost(reference.Domain(ref))]
	if !ok {
		return "", nil
	}
	buf, err := json.Marshal(ac)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(buf), nil
}

// normalizeHost turns config keys like "https://ghcr.io/v1/" into "ghcr.io"
// and maps all Docker Hub aliases to "docker.io".
func normalizeHost(s string) string {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s, _, _ = strings.Cut(s, "/")
	s = strings.ToLower(s)
	switch s {
	case "index.docker.io", "registry-1.docker.io", "docker.io":
		return "docker.io"
	}
	return s
}
