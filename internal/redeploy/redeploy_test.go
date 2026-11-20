package redeploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/skvostik/container-redeploy-webhook/internal/auth"
	"github.com/skvostik/container-redeploy-webhook/internal/labels"
)

const testToken = "test-token-0123456789"

// ---- fake docker -----------------------------------------------------------

type fakeDocker struct {
	containers map[string]*container.InspectResponse
	local      map[string]string // image ref -> local image ID
	remote     map[string]string // image ref -> image ID available in the registry
	imageCfg   map[string]*dockerspec.DockerOCIImageConfig
	pullErr    error
	failStart  bool // fail starting newly created containers
	nextID     int
	creates    []client.ContainerCreateOptions
	connects   []string
	pulls      []string
}

func newFake() *fakeDocker {
	return &fakeDocker{
		containers: map[string]*container.InspectResponse{},
		local:      map[string]string{},
		remote:     map[string]string{},
		imageCfg:   map[string]*dockerspec.DockerOCIImageConfig{},
	}
}

func (f *fakeDocker) byName(n string) *container.InspectResponse {
	for _, c := range f.containers {
		if c.Name == "/"+n {
			return c
		}
	}
	return nil
}

func (f *fakeDocker) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	var want []string
	for k := range o.Filters["label"] {
		want = append(want, k)
	}
	var res client.ContainerListResult
	for _, id := range slices.Sorted(maps.Keys(f.containers)) {
		c := f.containers[id]
		ok := true
		for _, w := range want {
			k, v, _ := strings.Cut(w, "=")
			if c.Config.Labels[k] != v {
				ok = false
			}
		}
		if ok {
			res.Items = append(res.Items, container.Summary{ID: id})
		}
	}
	return res, nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	c, ok := f.containers[id]
	if !ok {
		return client.ContainerInspectResult{}, cerrdefs.ErrNotFound
	}
	return client.ContainerInspectResult{Container: *c}, nil
}

func (f *fakeDocker) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if f.byName(o.Name) != nil {
		return client.ContainerCreateResult{}, cerrdefs.ErrConflict.WithMessage("name in use")
	}
	f.creates = append(f.creates, o)
	f.nextID++
	id := fmt.Sprintf("%064d", f.nextID)
	nets := map[string]*network.EndpointSettings{}
	if o.NetworkingConfig != nil {
		maps.Copy(nets, o.NetworkingConfig.EndpointsConfig)
	}
	f.containers[id] = &container.InspectResponse{
		ID: id, Name: "/" + o.Name, Image: f.local[o.Config.Image],
		Config: o.Config, HostConfig: o.HostConfig,
		State:           &container.State{Status: container.StateCreated},
		NetworkSettings: &container.NetworkSettings{Networks: nets},
	}
	return client.ContainerCreateResult{ID: id}, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	c, ok := f.containers[id]
	if !ok {
		return client.ContainerStartResult{}, cerrdefs.ErrNotFound
	}
	if f.failStart && c.State.Status == container.StateCreated {
		return client.ContainerStartResult{}, errors.New("boom: port already allocated")
	}
	c.State.Status = container.StateRunning
	return client.ContainerStartResult{}, nil
}

func (f *fakeDocker) ContainerStop(_ context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	c, ok := f.containers[id]
	if !ok {
		return client.ContainerStopResult{}, cerrdefs.ErrNotFound
	}
	c.State.Status = container.StateExited
	return client.ContainerStopResult{}, nil
}

func (f *fakeDocker) ContainerRename(_ context.Context, id string, o client.ContainerRenameOptions) (client.ContainerRenameResult, error) {
	c, ok := f.containers[id]
	if !ok {
		return client.ContainerRenameResult{}, cerrdefs.ErrNotFound
	}
	if other := f.byName(o.NewName); other != nil && other.ID != id {
		return client.ContainerRenameResult{}, cerrdefs.ErrConflict
	}
	c.Name = "/" + o.NewName
	return client.ContainerRenameResult{}, nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if _, ok := f.containers[id]; !ok {
		return client.ContainerRemoveResult{}, cerrdefs.ErrNotFound
	}
	delete(f.containers, id)
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeDocker) NetworkConnect(_ context.Context, n string, o client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	f.connects = append(f.connects, n)
	f.containers[o.Container].NetworkSettings.Networks[n] = o.EndpointConfig
	return client.NetworkConnectResult{}, nil
}

type fakePull struct{ io.Reader }

func (fakePull) Close() error { return nil }
func (fakePull) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] {
	return func(func(jsonstream.Message, error) bool) {}
}
func (fakePull) Wait(context.Context) error { return nil }

func (f *fakeDocker) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.pulls = append(f.pulls, ref)
	if f.pullErr != nil {
		return nil, f.pullErr
	}
	if id, ok := f.remote[ref]; ok {
		f.local[ref] = id
	}
	return fakePull{strings.NewReader("")}, nil
}

func (f *fakeDocker) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	id := ref
	if v, ok := f.local[ref]; ok {
		id = v
	}
	cfg, ok := f.imageCfg[id]
	if !ok {
		return client.ImageInspectResult{}, cerrdefs.ErrNotFound
	}
	return client.ImageInspectResult{InspectResponse: image.InspectResponse{ID: id, Config: cfg}}, nil
}

// ---- fixtures --------------------------------------------------------------

const (
	oldImg = "sha256:old"
	newImg = "sha256:new"
	ref    = "ghcr.io/skvostik/app:main"
	appID  = "aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
)

// setup creates a fake with one running container "app" (id "demo") whose
// config has the old image's defaults merged in, like a real inspect.
func setup(remoteID string) *fakeDocker {
	f := newFake()
	f.local[ref] = oldImg
	f.remote[ref] = remoteID
	imgCfg := func(version string) *dockerspec.DockerOCIImageConfig {
		return &dockerspec.DockerOCIImageConfig{
			ImageConfig: ocispec.ImageConfig{
				Env:          []string{"PATH=/usr/bin", "APP_VERSION=" + version},
				Cmd:          []string{"/app", "serve"},
				Labels:       map[string]string{"org.opencontainers.image.version": version},
				ExposedPorts: map[string]struct{}{"8080/tcp": {}},
				Volumes:      map[string]struct{}{"/data": {}},
				WorkingDir:   "/srv",
			},
		}
	}
	f.imageCfg[oldImg] = imgCfg("1")
	f.imageCfg[newImg] = imgCfg("2")

	f.containers[appID] = &container.InspectResponse{
		ID:    appID,
		Name:  "/app",
		Image: oldImg,
		State: &container.State{Status: container.StateRunning},
		Config: &container.Config{
			Hostname:     appID[:12],
			Image:        ref,
			Env:          []string{"PATH=/usr/bin", "APP_VERSION=1", "FOO=bar"},
			Cmd:          []string{"/app", "serve"},
			WorkingDir:   "/srv",
			ExposedPorts: network.PortSet{network.MustParsePort("8080/tcp"): {}, network.MustParsePort("9090/tcp"): {}},
			Volumes:      map[string]struct{}{"/data": {}},
			Labels: map[string]string{
				"org.opencontainers.image.version": "1",
				labels.ID:                          "demo",
				"com.docker.compose.service":       "app",
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: "proj_default",
			Binds:       []string{"/host/conf:/etc/app:ro"},
		},
		Mounts: []container.MountPoint{
			{Type: mount.TypeBind, Source: "/host/conf", Destination: "/etc/app"},
			{Type: mount.TypeVolume, Name: "anonvol123", Destination: "/data", RW: true},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"proj_default": {Aliases: []string{"app", appID[:12]}, NetworkID: "n1", EndpointID: "e1"},
			"extra":        {Aliases: []string{"api"}, NetworkID: "n2"},
		}},
	}
	return f
}

func newService(t *testing.T, f *fakeDocker, token string) (*Service, *auth.Principal) {
	t.Helper()
	a, err := auth.New(token, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := a.Authenticate(context.Background(), token)
	s := New(Options{Docker: f, Auth: a, Logger: slog.New(slog.DiscardHandler)})
	s.now = func() time.Time { return time.Unix(1700000000, 0) }
	return s, p
}

// ---- tests -----------------------------------------------------------------

func TestUpToDate(t *testing.T) {
	f := setup(oldImg)
	s, p := newService(t, f, testToken)
	res, err := s.Redeploy(context.Background(), "demo", p, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Containers[0].Status != StatusUpToDate || len(f.creates) != 0 {
		t.Fatalf("expected up-to-date without recreate, got %+v creates=%d", res.Containers, len(f.creates))
	}
	if len(f.pulls) != 1 {
		t.Fatalf("expected one pull, got %v", f.pulls)
	}
}

func TestRecreate(t *testing.T) {
	f := setup(newImg)
	s, p := newService(t, f, testToken)
	res, err := s.Redeploy(context.Background(), "demo", p, false)
	if err != nil {
		t.Fatal(err)
	}
	cr := res.Containers[0]
	if cr.Status != StatusUpdated || cr.OldImageID != oldImg || cr.NewImageID != newImg {
		t.Fatalf("unexpected result %+v", cr)
	}
	if _, ok := f.containers[appID]; ok {
		t.Fatal("old container not removed")
	}
	nc := f.byName("app")
	if nc == nil || nc.State.Status != container.StateRunning || nc.Image != newImg {
		t.Fatalf("new container not running with new image: %+v", nc)
	}
	if len(f.containers) != 1 {
		t.Fatalf("expected exactly one container, have %d", len(f.containers))
	}

	o := f.creates[0]
	cfg := o.Config
	if cfg.Hostname != "" {
		t.Errorf("auto hostname not cleared: %q", cfg.Hostname)
	}
	if !slices.Equal(cfg.Env, []string{"FOO=bar"}) {
		t.Errorf("env = %v, want only user env", cfg.Env)
	}
	if _, ok := cfg.Labels["org.opencontainers.image.version"]; ok {
		t.Errorf("old image label kept: %v", cfg.Labels)
	}
	if cfg.Labels[labels.ID] != "demo" || cfg.Labels["com.docker.compose.service"] != "app" {
		t.Errorf("user labels lost: %v", cfg.Labels)
	}
	if cfg.Cmd != nil || cfg.WorkingDir != "" {
		t.Errorf("image defaults not stripped: cmd=%v wd=%q", cfg.Cmd, cfg.WorkingDir)
	}
	if _, ok := cfg.ExposedPorts[network.MustParsePort("9090/tcp")]; !ok || len(cfg.ExposedPorts) != 1 {
		t.Errorf("exposed ports = %v, want only user port 9090", cfg.ExposedPorts)
	}
	if cfg.Image != ref {
		t.Errorf("image ref = %q", cfg.Image)
	}

	// networks: primary at create, extra connected afterwards, runtime fields dropped
	eps := o.NetworkingConfig.EndpointsConfig
	if len(eps) != 1 || eps["proj_default"] == nil {
		t.Fatalf("endpoints = %v", eps)
	}
	if ep := eps["proj_default"]; !slices.Equal(ep.Aliases, []string{"app"}) || ep.NetworkID != "" || ep.EndpointID != "" {
		t.Errorf("primary endpoint = %+v", ep)
	}
	if !slices.Equal(f.connects, []string{"extra"}) {
		t.Errorf("connects = %v", f.connects)
	}

	// anonymous volume carried over, bind not duplicated
	var anon []mount.Mount
	for _, m := range o.HostConfig.Mounts {
		if m.Type == mount.TypeVolume {
			anon = append(anon, m)
		}
	}
	if len(anon) != 1 || anon[0].Source != "anonvol123" || anon[0].Target != "/data" || anon[0].ReadOnly {
		t.Errorf("mounts = %+v", o.HostConfig.Mounts)
	}
	if len(o.HostConfig.Mounts) != 1 {
		t.Errorf("bind mount duplicated into Mounts: %+v", o.HostConfig.Mounts)
	}
}

func TestForce(t *testing.T) {
	f := setup(oldImg)
	s, p := newService(t, f, testToken)
	res, err := s.Redeploy(context.Background(), "demo", p, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Containers[0].Status != StatusUpdated || len(f.creates) != 1 {
		t.Fatalf("force did not recreate: %+v", res.Containers)
	}
}

func TestRollbackOnStartFailure(t *testing.T) {
	f := setup(newImg)
	f.failStart = true
	s, p := newService(t, f, testToken)
	res, err := s.Redeploy(context.Background(), "demo", p, false)
	if !errors.Is(err, ErrFailed) {
		t.Fatalf("expected ErrFailed, got %v", err)
	}
	if res == nil || res.Containers[0].Status != StatusFailed {
		t.Fatalf("result = %+v", res)
	}
	if len(f.containers) != 1 {
		t.Fatalf("new container not cleaned up: %d containers", len(f.containers))
	}
	old := f.containers[appID]
	if old == nil || old.Name != "/app" || old.State.Status != container.StateRunning {
		t.Fatalf("old container not restored: %+v", old)
	}
	if !strings.Contains(err.Error(), "old container restored") {
		t.Errorf("error does not mention rollback: %v", err)
	}
}

func TestRefusals(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*fakeDocker, *Service)
		token  string
		id     string
		want   error
	}{
		{"unknown id", nil, testToken, "nope", ErrNotFound},
		{"no auth method", nil, "", "demo", ErrUnauthorized},
		{"exited", func(f *fakeDocker, _ *Service) { f.containers[appID].State.Status = container.StateExited }, testToken, "demo", ErrConflict},
		{"paused", func(f *fakeDocker, _ *Service) { f.containers[appID].State.Status = container.StatePaused }, testToken, "demo", ErrConflict},
		{"created", func(f *fakeDocker, _ *Service) { f.containers[appID].State.Status = container.StateCreated }, testToken, "demo", ErrConflict},
		{"self", func(_ *fakeDocker, s *Service) { s.SelfID = appID }, testToken, "demo", ErrConflict},
		{"image id", func(f *fakeDocker, _ *Service) { f.containers[appID].Config.Image = "sha256:abc" }, testToken, "demo", ErrConflict},
		{"auto-remove", func(f *fakeDocker, _ *Service) { f.containers[appID].HostConfig.AutoRemove = true }, testToken, "demo", ErrConflict},
		{"pull error", func(f *fakeDocker, _ *Service) { f.pullErr = errors.New("denied") }, testToken, "demo", ErrFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(newImg)
			s, p := newService(t, f, tt.token)
			if tt.mutate != nil {
				tt.mutate(f, s)
			}
			_, err := s.Redeploy(context.Background(), tt.id, p, true)
			if !errors.Is(err, tt.want) {
				t.Fatalf("got %v, want %v", err, tt.want)
			}
			if len(f.creates) != 0 || f.containers[appID] == nil || f.containers[appID].Name != "/app" {
				t.Fatal("container was touched")
			}
		})
	}
}

func TestRestartingIsAllowed(t *testing.T) {
	f := setup(newImg)
	f.containers[appID].State.Status = container.StateRestarting
	s, p := newService(t, f, testToken)
	if _, err := s.Redeploy(context.Background(), "demo", p, false); err != nil {
		t.Fatal(err)
	}
}

func TestMultipleContainersShareID(t *testing.T) {
	f := setup(newImg)
	worker := *f.containers[appID]
	worker.ID = strings.Repeat("b", 64)
	worker.Name = "/worker"
	worker.State = &container.State{Status: container.StateRunning}
	f.containers[worker.ID] = &worker
	s, p := newService(t, f, testToken)
	res, err := s.Redeploy(context.Background(), "demo", p, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Containers) != 2 || len(f.pulls) != 1 {
		t.Fatalf("containers=%d pulls=%v", len(res.Containers), f.pulls)
	}
	for _, n := range []string{"app", "worker"} {
		if c := f.byName(n); c == nil || c.Image != newImg {
			t.Errorf("%s not updated", n)
		}
	}
}
