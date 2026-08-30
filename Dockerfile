# Build the web interface first: it is architecture-independent, so one build
# serves every target platform.
FROM --platform=$BUILDPLATFORM node:22-alpine AS ui

WORKDIR /ui
RUN corepack enable

# Dependencies are copied on their own so a source-only change does not
# reinstall them.
COPY ui/package.json ui/pnpm-lock.yaml* ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile

COPY ui/ ./
RUN pnpm build


# Cross-compile the binary on the build machine. CGO is off and the SQLite
# driver is pure Go, so producing an arm64 binary on an amd64 runner is an
# ordinary compile — no QEMU, which would otherwise turn a one-minute build
# into fifteen.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
COPY --from=ui /ui/dist ./ui/dist

ARG TARGETARCH
ARG VERSION=0.1
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/tdrive ./cmd/tdrive


# distroless/static is the smallest base that still carries CA certificates,
# which the Telegram connection and remote-URL fetches both need.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/tdrive /usr/local/bin/tdrive

# The data directory holds the SQLite index, the Telegram session and the
# upload spool. It is the only thing that needs to persist.
VOLUME ["/data"]
ENV TDRIVE_DATA_DIR=/data \
    TDRIVE_LISTEN=:8080

EXPOSE 8080

# /data may be a host bind mount. Its ownership is controlled by the host and
# cannot be fixed while building the image, so keep the process able to create
# and update the SQLite database on a fresh deployment.
USER root:root

ENTRYPOINT ["/usr/local/bin/tdrive"]


# The builder is a separate image target. Keeping Go, Git and a shell here
# means the main runtime stays distroless while source installation remains
# available to Docker deployments.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS plugin-builder-build

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" \
    -o /out/tdrive-plugin-builder ./cmd/tdrive-plugin-builder

FROM golang:1.26-alpine AS plugin-builder

RUN apk add --no-cache git ca-certificates

COPY --from=plugin-builder-build /out/tdrive-plugin-builder /usr/local/bin/tdrive-plugin-builder

VOLUME ["/plugins", "/run/tdrive-plugin-builder"]
ENV TDRIVE_PLUGIN_DIR=/plugins \
    TDRIVE_PLUGIN_BUILDER_ADDRESS=/run/tdrive-plugin-builder/plugin-builder.sock

ENTRYPOINT ["/usr/local/bin/tdrive-plugin-builder"]

# Keep `docker build .` pointed at the small runtime image. CI selects the
# `plugin-builder` target explicitly for the sidecar image.
FROM runtime AS final
