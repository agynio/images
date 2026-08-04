SHELL := /bin/bash

.PHONY: all proto build test lint fmt clean

all: build

# The images protos are vendored under proto/ until they land in
# buf.build/agynio/api; see proto/buf.yaml.
proto:
	buf generate

build:
	GOFLAGS=-mod=mod go build ./...

test:
	GOFLAGS=-mod=mod go test ./...

lint:
	GOFLAGS=-mod=mod go vet ./...

fmt:
	gofmt -w $(shell find . -type f -name '*.go' -not -path './gen/*')

clean:
	rm -rf gen
