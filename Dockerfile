# Copyright (c) 2026 suyash1603
# SPDX-License-Identifier: MPL-2.0

# Multi-stage build. `docker build --target=<name> .` selects a stage;
# the default is `dev`.

# certbuild captures the CA certificates, so the scratch image can verify TLS
# when talking to a Nomad cluster over HTTPS.
FROM alpine:3.22 AS certbuild
RUN apk add --no-cache ca-certificates

# devbuild compiles the binary.
# -----------------------------------
FROM golang:1.26-alpine AS devbuild
ARG VERSION="dev"
WORKDIR /build

# Dependencies first, so the module cache layer survives source edits.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build go mod download

COPY . ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -ldflags="-s -w" \
    -o nomad-mcp-server ./cmd/nomad-mcp-server

# dev runs the compiled binary from a scratch image.
# -----------------------------------
FROM scratch AS dev
WORKDIR /server
COPY --from=devbuild /build/nomad-mcp-server .
COPY --from=certbuild /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Reaching a Nomad agent on the host from inside the container needs
# host.docker.internal rather than 127.0.0.1, which would be the container.
ENV NOMAD_ADDR="http://host.docker.internal:4646"

# stdio is the default transport: `docker run -i --rm <img>` is what an MCP
# client launches. Read-only stays on unless the operator turns it off.
ENTRYPOINT ["./nomad-mcp-server"]
CMD ["stdio"]

# release uses a binary built by CI rather than compiling in-image.
# -----------------------------------
FROM scratch AS release
ARG BIN_NAME=nomad-mcp-server
ARG PRODUCT_VERSION
ARG PRODUCT_REVISION
ARG TARGETOS TARGETARCH
LABEL version=$PRODUCT_VERSION
LABEL revision=$PRODUCT_REVISION
COPY dist/$TARGETOS/$TARGETARCH/$BIN_NAME /bin/nomad-mcp-server
COPY --from=certbuild /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/bin/nomad-mcp-server"]
CMD ["stdio"]

# Default target.
# -----------------------------------
FROM dev
