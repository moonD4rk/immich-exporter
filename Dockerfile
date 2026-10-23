# Cross-compile natively on the build host (e.g. arm64) to the target arch.
# Base images are pinned by digest so Dependabot (docker ecosystem) can bump them.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:d530cf60723abf1ffae0cd16bb546b379ec0550d2f3815e5b16ed69dbc00fba4 AS build
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

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /immich-exporter /immich-exporter
EXPOSE 8000
USER 65532:65532
ENTRYPOINT ["/immich-exporter"]
