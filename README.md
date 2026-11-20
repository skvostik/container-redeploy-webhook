# container-redeploy-webhook

[![CI](https://github.com/skvostik/container-redeploy-webhook/actions/workflows/ci.yml/badge.svg)](https://github.com/skvostik/container-redeploy-webhook/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A tiny webhook that redeploys a Docker container: your CI calls `POST /redeploy/<id>`, and the
webhook pulls the new image and recreates the container with the same configuration.

- **Opt-in.** Only containers labelled `skvostik.redeploy.id` can be touched.
- **No secrets for GitHub Actions.** The job's OIDC token is checked against the container's labels.
- **Nothing gets lost.** Env, ports, volumes, networks, aliases, restart policy and Compose labels carry over.
- **Safe.** If Docker can't create or start the new container, the old one comes back. Stopped containers stay stopped.

## Quick start

**1. Run the webhook** and put it behind your HTTPS reverse proxy (e.g. at `https://deploy.example.com`):

```yaml
services:
  redeploy-webhook:
    image: ghcr.io/skvostik/container-redeploy-webhook:latest
    restart: unless-stopped
    stop_grace_period: 5m   # let a running redeploy finish when the webhook is updated
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
```

**2. Label the container** you want to redeploy:

```yaml
services:
  myapp:
    image: ghcr.io/your-org/myapp:latest
    restart: unless-stopped
    labels:
      skvostik.redeploy.id: myapp
      skvostik.redeploy.github.repository: your-org/myapp
```

**3. Call it from GitHub Actions** after pushing the image:

```yaml
jobs:
  deploy:
    needs: build
    runs-on: ubuntu-latest
    permissions:
      id-token: write
    steps:
      - uses: skvostik/container-redeploy-webhook@v0
        with:
          url: https://deploy.example.com
          id: myapp
```

Done, no secrets needed. More in [examples/](examples/): a full
[workflow](examples/github-workflow.yml), [Caddy](examples/Caddyfile),
[Traefik](examples/docker-compose.traefik.yml) and a [socket proxy](examples/docker-compose.socket-proxy.yml).

## Labels

| Label | Description |
|---|---|
| `skvostik.redeploy.id` | **Required.** The id used in `/redeploy/<id>`. Several containers can share one. |
| `skvostik.redeploy.github.repository` | Allow GitHub OIDC tokens from this repository: `owner/repo` or the numeric repository ID. |
| `skvostik.redeploy.github.ref` | Optional. Also require this git ref, e.g. `refs/heads/main`. |
| `skvostik.redeploy.github.environment` | Optional. Also require this GitHub environment, e.g. `production`. |

## Authentication

Requests need `Authorization: Bearer <token>`, which is either:

- **a GitHub Actions OIDC token** (recommended), accepted for containers whose `github.*` labels match
  the token's repository, ref and environment. The job needs `permissions: id-token: write`; the
  action above takes care of the rest.
- **the shared `TOKEN`** set on the webhook (at least 16 characters), accepted for every labelled
  container. Useful for other CI systems or manual calls; with the action, pass
  `token: ${{ secrets.REDEPLOY_TOKEN }}`. Unset means disabled.

A container with no usable method (no `github.repository` label and no `TOKEN`) can't be redeployed.

> [!TIP]
> Use the numeric repository ID rather than `owner/repo`: it survives renames and can't be taken over
> by someone recreating a deleted repository. Get it with `gh api repos/your-org/myapp --jq .id`.

Calling it without the action:

```sh
curl -fsS -X POST -H "Authorization: Bearer $REDEPLOY_TOKEN" https://deploy.example.com/redeploy/myapp
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `TOKEN` | *(disabled)* | Shared bearer token, min. 16 characters. |
| `GITHUB_AUDIENCE` | `container-redeploy-webhook` | Expected OIDC audience (the action's `audience` input). |
| `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker endpoint, e.g. `tcp://docker-proxy:2375`. |
| `DOCKER_CONFIG` | `/config` | Directory with a `config.json` holding registry credentials. |
| `REDEPLOY_TIMEOUT` | `5m` | Max. duration of one redeploy. Keep `stop_grace_period` at least this long. |
| `STOP_TIMEOUT` | *(container's own)* | Seconds to wait for the old container to stop. |
| `LISTEN_ADDR` | `:8080` | HTTP listen address. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |

## API

**`POST /redeploy/{id}`** redeploys every container with that id and waits until it's done.
Add `?force=1` to recreate even if the image didn't change.

| Status | Meaning |
|---|---|
| `200` | Done. Each container is `updated` or `up-to-date`. |
| `401` | Bad token, labels don't match, or unknown id (deliberately indistinguishable; see the log). |
| `409` | Refused: a container isn't running, was started with `--rm`, uses a bare image ID, or is the webhook itself. |
| `500` | Pull or recreate failed; the old container was restored when possible. |

```json
{
  "id": "myapp",
  "containers": [
    { "name": "myapp-web-1", "image": "ghcr.io/your-org/myapp:latest",
      "oldImageId": "sha256:0872fb3a…", "newImageId": "sha256:7aa3d3c5…", "status": "updated" }
  ]
}
```

**`GET /healthz`** returns `200`. The image has a built-in `HEALTHCHECK` too.

## How it works

1. **Check.** Find the labelled containers and check the token. All of them must be `running` or
   `restarting`, otherwise nothing is touched.
2. **Pull.** Pull the image while the old container keeps running. If the pull fails, nothing is
   touched. If the image ID didn't change (and no `force`), stop here.
3. **Replace.** For each container, one after another:
   1. rename the old container to `<name>-old-<timestamp>`, freeing its name,
   2. stop the old container (its downtime starts here),
   3. create the new container with the same name and config, and connect its networks,
   4. start the new container (its downtime ends here),
   5. remove the old container.

If step 3.2, 3.3 or 3.4 fails, the new container is removed, and the old one gets its name back
and is started again. The remaining containers with the same id are skipped.

The old container is stopped *before* the new one starts, so ports, static IPs and volumes are
free. Expect a short downtime: roughly the old container's stop time plus the new one's start time.

Rollback covers only what Docker reports. The webhook doesn't wait for a healthcheck, so an app
that starts and then crashes is not rolled back. Its restart policy applies instead.

Settings the old *image* brought along (its `ENV`, `CMD`, `LABEL`, …) are not copied, so the new
image's defaults apply.

**Stopped containers are refused**, because a deploy shouldn't start something an operator stopped.
Use a restart policy (`unless-stopped`, `on-failure`, …) so a crashing container stays in
`restarting` and you can still deploy a fix to it.

## Socket proxy

Docker socket access is root-equivalent. To limit it, use
[docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy) with `CONTAINERS=1`,
`IMAGES=1`, `NETWORKS=1` and `POST=1`, and set `DOCKER_HOST=tcp://docker-proxy:2375`.
See [examples/docker-compose.socket-proxy.yml](examples/docker-compose.socket-proxy.yml).

## Private registries

Run `docker login` on the host and mount the credentials:

```yaml
    volumes:
      - /root/.docker/config.json:/config/config.json:ro
```

Credential helpers (`credsStore`) aren't supported. For GHCR, use a token with `read:packages`.

## Good to know

- Expose the webhook only over HTTPS; it speaks plain HTTP.
- The request stays open until the redeploy is done, so your reverse proxy must wait that long.
  nginx gives up after 60 s (`proxy_read_timeout`), Cloudflare after 100 s. On a proxy timeout
  CI reports an error, but the redeploy still finishes.
- On shutdown the webhook waits for running redeploys. Docker kills it after 10 s by default, which
  can leave a half-done redeploy behind, so set `stop_grace_period` (`docker run --stop-timeout`)
  to at least `REDEPLOY_TIMEOUT`.
- OIDC tokens are short-lived but reusable until they expire. Restrict with `github.ref` or
  `github.environment` if only one branch or environment should deploy.
- The webhook can't redeploy itself. Update it with `docker compose pull && docker compose up -d`.
- Containers using `network_mode: container:…` of a redeployed container lose their network until recreated.
- MAC addresses aren't kept. Containers started with `--rm` are refused. Swarm services aren't supported.
- Requires Docker 19.03+ (API 1.40).

## Development

```sh
go test ./...
docker build -t container-redeploy-webhook:dev .
scripts/e2e.sh   # end-to-end test; only touches resources prefixed redeploy-e2e
```

## Release process

1. Tag the commit on `main` and push the tag:

   ```sh
   git tag v0.1.0
   git push origin v0.1.0
   ```

   The release workflow publishes `ghcr.io/skvostik/container-redeploy-webhook` as `0.1.0` and
   `0.1`, plus `latest` if it is the newest release. From 1.0 on it also publishes the major tag
   (`1`) if it is the newest release of that major. Every push to `main` publishes `edge`.

2. Move the action's major tag, so `uses: skvostik/container-redeploy-webhook@v0` gets the new
   action (skip this for a patch of an older line):

   ```sh
   git tag -f v0 v0.1.0
   git push -f origin v0
   ```
