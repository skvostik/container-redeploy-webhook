# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/container-redeploy-webhook ./cmd/container-redeploy-webhook

FROM scratch
LABEL org.opencontainers.image.title="container-redeploy-webhook" \
      org.opencontainers.image.description="Tiny webhook that pulls and recreates labelled Docker containers" \
      org.opencontainers.image.source="https://github.com/skvostik/container-redeploy-webhook" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/container-redeploy-webhook /container-redeploy-webhook
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/container-redeploy-webhook", "healthcheck"]
ENTRYPOINT ["/container-redeploy-webhook"]
