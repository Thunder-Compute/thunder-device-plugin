# syntax=docker/dockerfile:1

# Build stage: compiled from source so images are reproducible from a git ref.
FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build

ARG VERSION=dev
ARG COMMIT=unknown
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Download modules first so dependency layers cache across source edits.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/Thunder-Compute/thunder-device-plugin/internal/version.Version=${VERSION} \
        -X github.com/Thunder-Compute/thunder-device-plugin/internal/version.Commit=${COMMIT}" \
      -o /out/thunder-dra-operator ./cmd/thunder-dra-operator

FROM debian:bookworm-slim

ARG VERSION=dev
ARG COMMIT=unknown

LABEL org.opencontainers.image.title="thunder-dra-operator" \
      org.opencontainers.image.description="Thunder Compute DRA ResourceSlice inventory operator" \
      org.opencontainers.image.source="https://github.com/Thunder-Compute/thunder-device-plugin" \
      org.opencontainers.image.vendor="Thunder Compute" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/thunder-dra-operator /usr/local/bin/thunder-dra-operator

# The operator only talks to the Kubernetes and Thunder APIs.
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/thunder-dra-operator"]
