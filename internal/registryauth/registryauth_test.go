package registryauth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/moby/moby/api/types/registry"
)

func decode(t *testing.T, s string) registry.AuthConfig {
	t.Helper()
	raw, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	var ac registry.AuthConfig
	if err := json.Unmarshal(raw, &ac); err != nil {
		t.Fatal(err)
	}
	return ac
}

func TestLoadAndFor(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"auths":{
		"https://index.docker.io/v1/":{"auth":"` + base64.StdEncoding.EncodeToString([]byte("hubuser:hubpass")) + `"},
		"ghcr.io":{"username":"gh","password":"ghp_x"},
		"registry.example.com:5000":{"identitytoken":"tok"}
	}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3 {
		t.Fatalf("len=%d", s.Len())
	}

	cases := map[string]string{
		"nginx:latest":                       "hubuser",
		"docker.io/library/nginx":            "hubuser",
		"ghcr.io/skvostik/app:main":          "gh",
		"registry.example.com:5000/team/app": "",
	}
	for img, user := range cases {
		enc, err := s.For(img)
		if err != nil || enc == "" {
			t.Fatalf("%s: enc=%q err=%v", img, enc, err)
		}
		if got := decode(t, enc).Username; got != user {
			t.Errorf("%s: username=%q want %q", img, got, user)
		}
	}
	if enc, _ := s.For("quay.io/foo/bar"); enc != "" {
		t.Errorf("unexpected credentials for quay.io")
	}
	if ac := decode(t, mustFor(t, s, "registry.example.com:5000/x")); ac.IdentityToken != "tok" {
		t.Errorf("identity token not passed")
	}
}

func mustFor(t *testing.T, s *Store, img string) string {
	t.Helper()
	v, err := s.For(img)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestLoadMissing(t *testing.T) {
	s, err := Load(t.TempDir())
	if err != nil || s.Len() != 0 {
		t.Fatalf("missing config: len=%d err=%v", s.Len(), err)
	}
	if v, err := s.For("nginx"); v != "" || err != nil {
		t.Fatalf("expected no credentials")
	}
}
