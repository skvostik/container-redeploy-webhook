// Command container-redeploy-webhook is a small HTTP webhook that pulls the
// image of a labelled Docker container and recreates the container.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/moby/moby/client"

	"github.com/skvostik/container-redeploy-webhook/internal/auth"
	"github.com/skvostik/container-redeploy-webhook/internal/redeploy"
	"github.com/skvostik/container-redeploy-webhook/internal/registryauth"
	"github.com/skvostik/container-redeploy-webhook/internal/server"
)

var version = "dev"

type config struct {
	listenAddr     string
	token          string
	githubAudience string
	timeout        time.Duration
	stopTimeout    *int
	dockerConfig   string
	logLevel       slog.Level
}

func loadConfig() (*config, error) {
	c := &config{
		listenAddr:     env("LISTEN_ADDR", ":8080"),
		token:          os.Getenv("TOKEN"),
		githubAudience: env("GITHUB_AUDIENCE", auth.DefaultAudience),
		dockerConfig:   env("DOCKER_CONFIG", "/config"),
	}
	var err error
	if c.timeout, err = time.ParseDuration(env("REDEPLOY_TIMEOUT", "5m")); err != nil || c.timeout <= 0 {
		return nil, fmt.Errorf("invalid REDEPLOY_TIMEOUT %q", os.Getenv("REDEPLOY_TIMEOUT"))
	}
	if v := os.Getenv("STOP_TIMEOUT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid STOP_TIMEOUT %q (seconds)", v)
		}
		c.stopTimeout = &n
	}
	if err := c.logLevel.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		return nil, fmt.Errorf("invalid LOG_LEVEL: %w", err)
	}
	return c, nil
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "version", "--version", "-v":
			fmt.Println(version)
			return
		}
	}
	if err := run(); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.logLevel}))
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	authorizer, err := auth.New(cfg.token, auth.NewGitHubVerifier(cfg.githubAudience))
	if err != nil {
		return err
	}

	registry, err := registryauth.Load(cfg.dockerConfig)
	if err != nil {
		return fmt.Errorf("registry credentials: %w", err)
	}

	docker, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer docker.Close()
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if v, err := docker.ServerVersion(pctx, client.ServerVersionOptions{}); err != nil {
		log.Warn("docker is not reachable yet", "host", docker.DaemonHost(), "err", err)
	} else {
		log.Info("connected to docker", "host", docker.DaemonHost(), "version", v.Version, "api", v.APIVersion)
	}
	cancel()

	svc := redeploy.New(redeploy.Options{
		Docker:      docker,
		Auth:        authorizer,
		Registry:    registry,
		Logger:      log,
		SelfID:      redeploy.SelfContainerID(),
		StopTimeout: cfg.stopTimeout,
	})
	srv := &server.Server{Auth: authorizer, Service: svc, Logger: log, Timeout: cfg.timeout}

	httpSrv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.timeout + 30*time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	log.Info("starting container-redeploy-webhook",
		"version", version,
		"listen", cfg.listenAddr,
		"token_auth", cfg.token != "",
		"github_audience", cfg.githubAudience,
		"registry_credentials", registry.Len(),
	)
	if cfg.token == "" {
		log.Info("TOKEN is not set: only containers with a GitHub OIDC label can be redeployed")
	}

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down, waiting for running redeploys")
	sctx, scancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer scancel()
	if err := httpSrv.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthcheck is used by the Docker HEALTHCHECK of the scratch image.
func healthcheck() int {
	addr := env("LISTEN_ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
