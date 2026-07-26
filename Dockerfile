# syntax=docker/dockerfile:1.7

# The embedded Copilot CLI version. `go tool bundler` otherwise resolves this
# from registry.npmjs.org at build time, which makes the image contents depend
# on when it was built. Keep it in sync with the version pinned by
# github.com/github/copilot-sdk/go in go.mod (`go tool bundler -check-only`
# verifies the match).
ARG COPILOT_CLI_VERSION=1.0.69

# Pinned by digest: `golang:1.26` is a moving tag, so an unpinned builder makes
# the toolchain a function of the build date. This digest is the multi-platform
# index, so --platform=$BUILDPLATFORM still resolves per builder architecture.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:3aff6657219a4d9c14e27fb1d8976c49c29fddb70ba835014f477e1c70636647 AS build
ARG TARGETOS
ARG TARGETARCH
ARG COPILOT_CLI_VERSION
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# Build an embedded Copilot CLI and application binary for the selected target.
RUN go tool bundler --platform "${TARGETOS}/${TARGETARCH}" --cli-version "${COPILOT_CLI_VERSION}" --output cmd/copilot-api
RUN CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -trimpath -o /out/copilot-api ./cmd/copilot-api

# Scaffold the runtime state tree here, because the runtime image has no shell.
# Docker only propagates ownership into a fresh named volume when the mountpoint
# already exists in the image; when it does not, Docker creates it as root:root
# 0755 and the uid-65532 process cannot chmod it, so startup dies with
# "operation not permitted".
#
# .cache holds three separate things and is therefore mounted as a whole:
#   copilot-api/  the managed cache root (prune/purge territory)
#   copilot-sdk/  where the Go SDK unpacks the embedded CLI binary
#   copilot/      where that CLI's Node single-executable loader then extracts
#                 its own bundled package and native addons, created at runtime
RUN mkdir -p /home/nonroot/.local/share/copilot-api \
             /home/nonroot/.local/state/copilot-api \
             /home/nonroot/.cache/copilot-api \
             /home/nonroot/.cache/copilot-sdk \
             /home/nonroot/.config/copilot-api

# cc, not static: the Go binary is CGO_ENABLED=0 and would be happy on static,
# but the embedded Copilot CLI the SDK unpacks and execs is a dynamically linked
# Node single-executable that requests /lib/ld-linux-*.so and links
# libc/libm/libdl/libpthread *and* libstdc++.so.6 + libgcc_s.so.1. static has no
# loader at all ("exec: no such file or directory") and base has glibc but no
# libstdc++ ("error while loading shared libraries: libstdc++.so.6"). cc is the
# smallest distroless variant that satisfies it.
FROM gcr.io/distroless/cc-debian12:nonroot@sha256:fccdbb0a547c14e23fcf4ce8ad62ca5d43b4faae8d22cd292f490fef9946c96e
ARG COPILOT_CLI_VERSION
LABEL org.opencontainers.image.title="copilot-api" \
      org.opencontainers.image.description="OpenAI-compatible API over the GitHub Copilot SDK" \
      org.opencontainers.image.source="https://github.com/evanlouie/copilot-api" \
      com.github.copilot.cli.version="${COPILOT_CLI_VERSION}"
COPY --from=build /out/copilot-api /usr/local/bin/copilot-api
COPY --from=build --chown=65532:65532 /home/nonroot /home/nonroot
# HOME is not set in the distroless image config; Docker infers it from
# /etc/passwd, but not every runtime does. Set it so the XDG lookups in
# internal/config resolve under /home/nonroot rather than falling back to
# /tmp/xdg-*. XDG_CACHE_HOME is honoured by internal/config's cache root, by the
# SDK's os.UserCacheDir() lookup, and by the CLI's own Node loader, so it pins
# all three cache consumers under /home/nonroot/.cache.
# COPILOT_API_ADDR defaults to 127.0.0.1:8080, which makes a published port
# unreachable; bind all interfaces instead. cmd/copilot-api requires
# COPILOT_API_KEY for any non-loopback bind, so the container refuses to start
# unauthenticated.
ENV HOME=/home/nonroot \
    XDG_CACHE_HOME=/home/nonroot/.cache \
    COPILOT_API_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/copilot-api"]
CMD ["serve"]
