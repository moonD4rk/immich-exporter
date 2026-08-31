# Cross-compile natively on the build host (e.g. arm64) to the target arch.
# Base images are pinned by digest so Dependabot (docker ecosystem) can bump them.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
WORKDIR /src
# Modules stay in this layer (not a cache mount) so the registry/GHA layer cache
# restores them across CI runs; the compile cache below only helps local rebuilds.
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS TARGETARCH VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /immich-exporter ./cmd/immich-exporter

FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7
COPY --from=build /immich-exporter /immich-exporter
EXPOSE 8000
USER 65532:65532
ENTRYPOINT ["/immich-exporter"]
