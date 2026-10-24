SHELL := /bin/sh
export GOPATH := $(CURDIR)/.cache/go
export GOMODCACHE := $(CURDIR)/.cache/go-mod
export GOCACHE := $(CURDIR)/.cache/go-build
export npm_config_cache := $(CURDIR)/.cache/npm
TARGET_ARCH ?= amd64

.PHONY: deps web build run test check config retention verify browser linux clean

deps:
	go mod download
	npm ci --ignore-scripts

web:
	npm run check:web
	npm run build:web

build: web
	mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/kosha ./cmd/kosha

run: build
	./bin/kosha

test: web
	go test -race ./...
	go vet ./...

check: test
	git diff --check

config: build
	npm run test:config

retention: build
	npm run test:retention

verify: build
	npm run test:config
	npm run test:retention
	npm run test:integration -- --duration=60

browser: build
	npm run test:browser

linux: web
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(TARGET_ARCH) go build -trimpath -o bin/kosha-linux-$(TARGET_ARCH) ./cmd/kosha

clean:
	rm -rf bin web/dist
