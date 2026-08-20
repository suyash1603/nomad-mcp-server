# Copyright (c) 2026 suyash1603
# SPDX-License-Identifier: MPL-2.0

SHELL := /usr/bin/env bash -euo pipefail -c

BINARY_NAME ?= ./bin/nomad-mcp-server
BASENAME := $(shell basename $(BINARY_NAME))
MODULE := github.com/suyash1603/nomad-mcp-server
VERSION ?= $(if $(shell printenv VERSION),$(shell printenv VERSION),dev)

GO = go
DOCKER = docker

DOCKER_REGISTRY ?= docker.io
IMAGE_NAME = $(DOCKER_REGISTRY)/$(BASENAME):$(VERSION)

TARGET_DIR ?= $(CURDIR)/dist

# Build metadata is injected rather than computed at runtime, so that a binary
# reports the commit it was actually built from. BuildDate is the HEAD commit's
# date, not the wall clock, which keeps builds reproducible.
GIT_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell git show --no-show-signature -s --format=%cd --date=format:"%Y-%m-%dT%H:%M:%SZ" HEAD 2>/dev/null || echo unknown)
LDFLAGS = -ldflags="-s -w -X $(MODULE)/version.GitCommit=$(GIT_COMMIT) -X $(MODULE)/version.BuildDate=$(BUILD_DATE)"

# Local ARCH; on Intel Mac 'uname -m' returns x86_64, which Go calls amd64.
# Deliberately not using 'go env GOOS/GOARCH' so the docker targets work
# without a local Go install. CGO is always off, for a static binary.
ARCH = $(shell A=$$(uname -m); [ $$A = x86_64 ] && A=amd64; echo $$A)
OS   = $(shell uname | tr '[:upper:]' '[:lower:]')

.PHONY: all build clean deps fmt vet lint test test-race test-e2e test-http \
        docker-build docker-push docker-run-http run-stdio run-http run-http-secure \
        inspector tidy check help

all: build

## build: compile the server into ./bin
build:
	CGO_ENABLED=0 GOARCH=$(ARCH) GOOS=$(OS) $(GO) build $(LDFLAGS) -o $(BINARY_NAME) ./cmd/$(BASENAME)

## clean: remove build artifacts
clean:
	rm -rf $(BINARY_NAME) $(TARGET_DIR)
	$(GO) clean

## deps: download dependencies
deps:
	$(GO) mod download

## tidy: prune and update go.mod / go.sum
tidy:
	$(GO) mod tidy

## fmt: format all Go source
fmt:
	$(GO) fmt ./...

## vet: run go vet
vet:
	$(GO) vet ./...

## lint: run golangci-lint if it is installed
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint not installed; skipping. Install: https://golangci-lint.run/welcome/install/"; \
	fi

## test: run unit tests
test:
	$(GO) test -v ./...

## test-race: run unit tests under the race detector
test-race:
	$(GO) test -race ./...

## test-e2e: run end-to-end tests against a real 'nomad agent -dev'
test-e2e:
	$(GO) test -v -tags e2e -timeout 10m ./e2e

## test-http: check the health endpoint of a locally running HTTP server
test-http:
	@echo "Testing StreamableHTTP health endpoint..."
	@curl -fsS http://127.0.0.1:8080/health && echo \
		|| echo "Health check failed - is the server running? Try 'make run-http'"
	@echo "MCP endpoint: http://127.0.0.1:8080/mcp"

## check: everything CI runs
check: fmt vet test

## run-stdio: run the server on stdio against a local dev agent
run-stdio: build
	$(BINARY_NAME) stdio

## run-http: run the StreamableHTTP server on port 8080
run-http: build
	$(BINARY_NAME) streamable-http --transport-port 8080

## run-http-secure: run the HTTP server with CORS locked down
run-http-secure: build
	MCP_ALLOWED_ORIGINS="http://localhost:3000" MCP_CORS_MODE="development" \
		$(BINARY_NAME) streamable-http --transport-port 8080 --transport-host 127.0.0.1

## inspector: launch the MCP Inspector against a freshly built binary
inspector: build
	npx @modelcontextprotocol/inspector $(BINARY_NAME)

## docker-build: build the docker image
docker-build:
	$(DOCKER) build --build-arg VERSION=$(VERSION) -t $(IMAGE_NAME) .

## docker-push: push the docker image
docker-push: docker-build
	$(DOCKER) push $(IMAGE_NAME)

## docker-run-http: run the HTTP server inside docker on port 8080
docker-run-http: docker-build
	$(DOCKER) run -p 8080:8080 --rm $(IMAGE_NAME) streamable-http \
		--transport-port 8080 --transport-host 0.0.0.0

## help: list available targets
help:
	@echo "Available targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /' | awk -F': ' '{printf "  %-18s %s\n", $$1, $$2}'
