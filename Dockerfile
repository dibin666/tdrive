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
# which the Telegram connection, remote-URL fetches and plugin downloads all
# need. Plugins are installed as prebuilt release binaries, so this image needs
# no Go toolchain, no Git and no shell — one container runs the whole product.
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
