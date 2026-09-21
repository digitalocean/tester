SHELL := /bin/bash

commit ?= $(shell git rev-parse --short HEAD)
image := registry.digitalocean.com/do-e2e-canaries/tester

include ./dev/dev.mk

.PHONY: clean
clean:
	rm -rf dist

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ./dist/tester-linux-amd64 ./cmd/tester/...

.PHONY: test
test:
	go vet ./...
	go test -race -cover ./...

.PHONY: generate
generate:
	go generate ./...

.PHONY: build-image
build-image:
	docker build -t $(image):sha-$(commit) .
ifdef LATEST
	docker tag $(image):sha-$(commit) $(image):edge
endif
ifdef PUSH
	docker push $(image):sha-$(commit)
	docker push $(image):edge
endif

.PHONY: install
install:
	go install ./cmd/tester/...
