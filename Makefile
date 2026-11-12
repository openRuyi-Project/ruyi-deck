ARCH ?= amd64
VERSION ?= dev
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CLI_PACKAGE := github.com/openRuyi-Project/ruyi-deck/internal/cli

.PHONY: build fmt

# Linux is the only runtime, including when cross-compiling from macOS.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -trimpath -ldflags "-s -w -X $(CLI_PACKAGE).Version=$(VERSION) -X $(CLI_PACKAGE).GitCommit=$(GIT_COMMIT) -X $(CLI_PACKAGE).BuildDate=$(BUILD_DATE)" -o bin/ruyi-deck-linux-$(ARCH) ./cmd/ruyi-deck

# Formatting does not execute the program or tests.
fmt:
	gofmt -w cmd internal templates/templates.go
