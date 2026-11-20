// Package redeploy pulls the image of labelled containers and recreates them
// with an identical configuration.
package redeploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/skvostik/container-redeploy-webhook/internal/auth"
	"github.com/skvostik/container-redeploy-webhook/internal/labels"
	"github.com/skvostik/container-redeploy-webhook/internal/registryauth"
)

// Docker is the subset of the Docker client used by the service.
// *client.Client satisfies it.
type Docker interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, id string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, id string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, id string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRename(ctx context.Context, id string, options client.ContainerRenameOptions) (client.ContainerRenameResult, error)
	ContainerRemove(ctx context.Context, id string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	NetworkConnect(ctx context.Context, network string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
	ImagePull(ctx context.Context, ref string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	ImageInspect(ctx context.Context, image string, opts ...client.ImageInspectOption) (client.ImageInspectResult, error)
}

// Container statuses reported in a Result.
const (
	StatusUpdated  = "updated"
	StatusUpToDate = "up-to-date"
	StatusFailed   = "failed"
	StatusSkipped  = "skipped"
)

// ContainerResult describes what happened to one container.
type ContainerResult struct {
	Name       string `json:"name"`
	Image      string `json:"image"`
	OldImageID string `json:"oldImageId"`
	NewImageID string `json:"newImageId,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

// Result is the outcome of a redeploy request.
type Result struct {
	ID         string            `json:"id"`
	Containers []ContainerResult `json:"containers"`
}

// Error kinds returned by Redeploy. Use errors.Is.
var (
	ErrNotFound     = errors.New("no container with this id")
	ErrUnauthorized = errors.New("unauthorized")
	ErrConflict     = errors.New("conflict")
	ErrFailed       = errors.New("redeploy failed")
)

// Options configure a Service.
type Options struct {
	Docker      Docker
	Auth        *auth.Authorizer
	Registry    *registryauth.Store
	Logger      *slog.Logger
	SelfID      string // ID of the webhook's own container, if any
	StopTimeout *int   // seconds; nil = container/daemon default
}

// Service performs redeploys.
type Service struct {
	Options
	now   func() time.Time
	mu    sync.Mutex
	locks map[string]chan struct{}
}

// New returns a Service.
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Service{Options: o, now: time.Now, locks: map[string]chan struct{}{}}
}

// lock serializes redeploys of the same id.
func (s *Service) lock(ctx context.Context, id string) (func(), error) {
	s.mu.Lock()
	ch, ok := s.locks[id]
	if !ok {
		ch = make(chan struct{}, 1)
		s.locks[id] = ch
	}
	s.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Redeploy redeploys every container labelled with id. p is the
// authenticated caller (may be nil). force recreates containers even when
// the image did not change.
func (s *Service) Redeploy(ctx context.Context, id string, p *auth.Principal, force bool) (*Result, error) {
	log := s.Logger.With("id", id, "caller", p.String())

	// Check authorization before taking the lock so unauthenticated callers
	// cannot block legitimate deploys.
	targets, err := s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		log.Warn("rejected: no container with this id")
		return nil, ErrNotFound
	}
	for _, c := range targets {
		if ok, reason := s.Auth.Allows(p, c.Config.Labels); !ok {
			log.Warn("rejected: unauthorized", "container", name(c), "reason", reason)
			return nil, ErrUnauthorized
		}
	}

	unlock, err := s.lock(ctx, id)
	if err != nil {
		return nil, err
	}
	defer unlock()

	// Re-read the containers: a previous redeploy may have replaced them while we waited.
	targets, err = s.find(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, ErrNotFound
	}

	for _, c := range targets {
		if ok, reason := s.Auth.Allows(p, c.Config.Labels); !ok {
			log.Warn("rejected: unauthorized", "container", name(c), "reason", reason)
			return nil, ErrUnauthorized
		}
		if s.SelfID != "" && c.ID == s.SelfID {
			return nil, fmt.Errorf("%w: refusing to redeploy the webhook's own container %s", ErrConflict, name(c))
		}
		st := container.ContainerState("")
		if c.State != nil {
			st = c.State.Status
		}
		if st != container.StateRunning && st != container.StateRestarting {
			return nil, fmt.Errorf("%w: container %s is %s (only running or restarting containers are redeployed)", ErrConflict, name(c), st)
		}
		if c.HostConfig.AutoRemove {
			return nil, fmt.Errorf("%w: container %s was started with --rm; stopping it would delete it and its anonymous volumes", ErrConflict, name(c))
		}
		if strings.HasPrefix(c.Config.Image, "sha256:") {
			return nil, fmt.Errorf("%w: container %s was created from an image ID, there is nothing to pull", ErrConflict, name(c))
		}
	}

	// Pull every image first; nothing is touched if a pull fails.
	newIDs := map[string]string{}
	for _, c := range targets {
		ref := c.Config.Image
		if _, done := newIDs[ref]; done {
			continue
		}
		log.Info("pulling image", "image", ref)
		imgID, err := s.pull(ctx, ref)
		if err != nil {
			log.Error("pull failed", "image", ref, "err", err)
			return nil, fmt.Errorf("%w: pull %s: %v", ErrFailed, ref, err)
		}
		newIDs[ref] = imgID
	}

	res := &Result{ID: id}
	var failed error
	for _, c := range targets {
		cr := ContainerResult{Name: name(c), Image: c.Config.Image, OldImageID: c.Image, NewImageID: newIDs[c.Config.Image]}
		switch {
		case failed != nil:
			cr.Status = StatusSkipped
		case cr.NewImageID == cr.OldImageID && !force:
			cr.Status = StatusUpToDate
			log.Info("image unchanged, container left alone", "container", cr.Name)
		default:
			log.Info("recreating container", "container", cr.Name, "old_image", short(cr.OldImageID), "new_image", short(cr.NewImageID))
			if err := s.recreate(ctx, c); err != nil {
				log.Error("recreate failed", "container", cr.Name, "err", err)
				cr.Status, cr.Error = StatusFailed, err.Error()
				failed = fmt.Errorf("%w: %s: %v", ErrFailed, cr.Name, err)
			} else {
				cr.Status = StatusUpdated
				log.Info("container recreated", "container", cr.Name)
			}
		}
		res.Containers = append(res.Containers, cr)
	}
	return res, failed
}

// find returns full inspect data of all containers labelled with id.
func (s *Service) find(ctx context.Context, id string) ([]container.InspectResponse, error) {
	list, err := s.Docker.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labels.ID+"="+id),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: list containers: %v", ErrFailed, err)
	}
	out := make([]container.InspectResponse, 0, len(list.Items))
	for _, item := range list.Items {
		insp, err := s.Docker.ContainerInspect(ctx, item.ID, client.ContainerInspectOptions{})
		if err != nil {
			return nil, fmt.Errorf("%w: inspect %s: %v", ErrFailed, item.ID, err)
		}
		c := insp.Container
		if c.Config == nil || c.HostConfig == nil || c.Config.Labels[labels.ID] != id {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

// pull pulls ref and returns the resulting local image ID.
func (s *Service) pull(ctx context.Context, ref string) (string, error) {
	regAuth, err := s.Registry.For(ref)
	if err != nil {
		return "", err
	}
	resp, err := s.Docker.ImagePull(ctx, ref, client.ImagePullOptions{RegistryAuth: regAuth})
	if err != nil {
		return "", err
	}
	if err := resp.Wait(ctx); err != nil {
		return "", err
	}
	img, err := s.Docker.ImageInspect(ctx, ref)
	if err != nil {
		return "", err
	}
	return img.ID, nil
}

func name(c container.InspectResponse) string { return strings.TrimPrefix(c.Name, "/") }

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
