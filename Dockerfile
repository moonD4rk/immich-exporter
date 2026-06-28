# Cross-compile natively on the build host (e.g. arm64) to the target arch.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /immich-exporter ./cmd/immich-exporter

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /immich-exporter /immich-exporter
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/immich-exporter"]
