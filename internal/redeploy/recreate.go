package redeploy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// recreate replaces container c with a new one created from the same
// configuration and the freshly pulled image. On failure the old container
// is restored.
//
// Order: rename old -> stop old -> create new -> connect extra networks ->
// start new -> remove old. Renaming first frees the name and keeps the old
// container around for rollback. --rm containers are refused earlier: stopping
// them would delete them.
func (s *Service) recreate(ctx context.Context, c container.InspectResponse) (err error) {
	origName := name(c)
	oldName := fmt.Sprintf("%s-old-%d", origName, s.now().Unix())

	spec, err := s.buildSpec(ctx, c)
	if err != nil {
		return err
	}

	if _, err := s.Docker.ContainerRename(ctx, c.ID, client.ContainerRenameOptions{NewName: oldName}); err != nil {
		return fmt.Errorf("rename old container: %w", err)
	}

	var newID string
	defer func() {
		if err == nil {
			return
		}
		// Use a fresh context: the request context may be what failed.
		rctx := context.WithoutCancel(ctx)
		if newID != "" {
			if _, rerr := s.Docker.ContainerRemove(rctx, newID, client.ContainerRemoveOptions{Force: true}); rerr != nil {
				err = errors.Join(err, fmt.Errorf("rollback: remove new container: %w", rerr))
			}
		}
		if _, rerr := s.Docker.ContainerRename(rctx, c.ID, client.ContainerRenameOptions{NewName: origName}); rerr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: rename old container back: %w", rerr))
			return
		}
		if _, rerr := s.Docker.ContainerStart(rctx, c.ID, client.ContainerStartOptions{}); rerr != nil {
			err = errors.Join(err, fmt.Errorf("rollback: start old container: %w", rerr))
			return
		}
		err = fmt.Errorf("%w (old container restored)", err)
	}()

	if _, err := s.Docker.ContainerStop(ctx, c.ID, client.ContainerStopOptions{Timeout: s.StopTimeout}); err != nil {
		return fmt.Errorf("stop old container: %w", err)
	}

	created, err := s.Docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:             origName,
		Config:           spec.config,
		HostConfig:       spec.hostConfig,
		NetworkingConfig: spec.networking,
	})
	if err != nil {
		return fmt.Errorf("create new container: %w", err)
	}
	newID = created.ID

	for _, n := range spec.extraNetworks {
		if _, err := s.Docker.NetworkConnect(ctx, n.name, client.NetworkConnectOptions{Container: newID, EndpointConfig: n.endpoint}); err != nil {
			return fmt.Errorf("connect network %s: %w", n.name, err)
		}
	}

	if _, err := s.Docker.ContainerStart(ctx, newID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start new container: %w", err)
	}

	// The new container is running; failing to clean up the old one is not fatal.
	if _, rerr := s.Docker.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{Force: true}); rerr != nil && !cerrdefs.IsNotFound(rerr) {
		s.Logger.Warn("could not remove old container", "container", oldName, "err", rerr)
	}
	return nil
}

type namedEndpoint struct {
	name     string
	endpoint *network.EndpointSettings
}

type createSpec struct {
	config        *container.Config
	hostConfig    *container.HostConfig
	networking    *network.NetworkingConfig
	extraNetworks []namedEndpoint
}

// buildSpec derives the create request for the replacement container.
func (s *Service) buildSpec(ctx context.Context, c container.InspectResponse) (*createSpec, error) {
	cfg := *c.Config
	cfg.Labels = maps.Clone(c.Config.Labels)
	cfg.Env = slices.Clone(c.Config.Env)
	cfg.ExposedPorts = maps.Clone(c.Config.ExposedPorts)
	cfg.Volumes = maps.Clone(c.Config.Volumes)

	// An auto-generated hostname is the short container ID; let Docker generate a new one.
	if len(c.ID) >= 12 && cfg.Hostname == c.ID[:12] {
		cfg.Hostname = ""
	}

	// The inspected config already has the OLD image's defaults merged in.
	// Drop everything that came from the old image so the new image's
	// defaults (ENV, LABEL, CMD, ...) take effect.
	if img, err := s.Docker.ImageInspect(ctx, c.Image); err != nil {
		s.Logger.Warn("cannot inspect old image, keeping inherited image config", "container", name(c), "err", err)
	} else if img.Config != nil {
		stripImageDefaults(&cfg, img)
	}

	hc := *c.HostConfig
	hc.Mounts = append(slices.Clone(c.HostConfig.Mounts), anonymousVolumes(c)...)

	spec := &createSpec{config: &cfg, hostConfig: &hc}

	mode := c.HostConfig.NetworkMode
	if mode.IsHost() || mode.IsNone() || mode.IsContainer() || c.NetworkSettings == nil || len(c.NetworkSettings.Networks) == 0 {
		return spec, nil
	}

	// Create with one network (older API versions accept only one), connect
	// the rest before starting.
	netNames := slices.Collect(maps.Keys(c.NetworkSettings.Networks))
	sort.Strings(netNames)
	primary := netNames[0]
	if _, ok := c.NetworkSettings.Networks[string(mode)]; ok {
		primary = string(mode)
	} else if mode.IsDefault() {
		if _, ok := c.NetworkSettings.Networks[network.NetworkBridge]; ok {
			primary = network.NetworkBridge
		}
	}
	spec.networking = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		primary: endpointConfig(c.NetworkSettings.Networks[primary], c.ID),
	}}
	for _, n := range netNames {
		if n != primary {
			spec.extraNetworks = append(spec.extraNetworks, namedEndpoint{n, endpointConfig(c.NetworkSettings.Networks[n], c.ID)})
		}
	}
	return spec, nil
}

// endpointConfig keeps the user-supplied part of an endpoint and drops
// runtime data (IDs, assigned addresses, generated MAC).
func endpointConfig(e *network.EndpointSettings, containerID string) *network.EndpointSettings {
	if e == nil {
		return &network.EndpointSettings{}
	}
	var aliases []string
	for _, a := range e.Aliases {
		// Older daemons add the short container ID as an alias.
		if len(containerID) >= 12 && a == containerID[:12] {
			continue
		}
		aliases = append(aliases, a)
	}
	return &network.EndpointSettings{
		IPAMConfig: e.IPAMConfig,
		Links:      slices.Clone(e.Links),
		Aliases:    aliases,
		DriverOpts: maps.Clone(e.DriverOpts),
		GwPriority: e.GwPriority,
	}
}

// anonymousVolumes returns mounts that re-attach the old container's
// anonymous volumes (from image VOLUME or `-v /path`) to the new container,
// so their data is kept, like `docker compose` does.
func anonymousVolumes(c container.InspectResponse) []mount.Mount {
	explicit := map[string]bool{}
	for _, b := range c.HostConfig.Binds {
		parts := strings.Split(b, ":")
		if len(parts) >= 2 {
			explicit[parts[1]] = true
		}
	}
	for _, m := range c.HostConfig.Mounts {
		explicit[m.Target] = true
	}
	var out []mount.Mount
	for _, m := range c.Mounts {
		if m.Type != mount.TypeVolume || m.Name == "" || explicit[m.Destination] {
			continue
		}
		out = append(out, mount.Mount{Type: mount.TypeVolume, Source: m.Name, Target: m.Destination, ReadOnly: !m.RW})
	}
	return out
}

// stripImageDefaults removes values from cfg that are identical to the old
// image's defaults.
func stripImageDefaults(cfg *container.Config, img client.ImageInspectResult) {
	ic := img.Config

	imgEnv := map[string]bool{}
	for _, e := range ic.Env {
		imgEnv[e] = true
	}
	cfg.Env = slices.DeleteFunc(cfg.Env, func(e string) bool { return imgEnv[e] })

	for k, v := range ic.Labels {
		if cv, ok := cfg.Labels[k]; ok && cv == v {
			delete(cfg.Labels, k)
		}
	}
	for p := range ic.ExposedPorts {
		if port, err := network.ParsePort(p); err == nil {
			delete(cfg.ExposedPorts, port)
		}
	}
	for v := range ic.Volumes {
		delete(cfg.Volumes, v)
	}
	if slices.Equal(cfg.Cmd, ic.Cmd) {
		cfg.Cmd = nil
	}
	if slices.Equal(cfg.Entrypoint, ic.Entrypoint) {
		cfg.Entrypoint = nil
	}
	if cfg.WorkingDir == ic.WorkingDir {
		cfg.WorkingDir = ""
	}
	if cfg.User == ic.User {
		cfg.User = ""
	}
	if cfg.StopSignal == ic.StopSignal {
		cfg.StopSignal = ""
	}
	if cfg.Healthcheck != nil && ic.Healthcheck != nil && reflect.DeepEqual(*cfg.Healthcheck, *ic.Healthcheck) {
		cfg.Healthcheck = nil
	}
	if slices.Equal(cfg.Shell, ic.Shell) {
		cfg.Shell = nil
	}
	if slices.Equal(cfg.OnBuild, ic.OnBuild) {
		cfg.OnBuild = nil
	}
	if len(cfg.Labels) == 0 {
		cfg.Labels = nil
	}
}
