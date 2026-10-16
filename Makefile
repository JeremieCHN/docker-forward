BINARY   := docker-forward
PKG      := ./cmd/docker-forward
VERSION  ?= 1.0.0
IMAGE    ?= docker-forward:latest
GOPROXY  ?=

GOFLAGS  := -trimpath
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt tidy clean image

all: build

build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o ./$(BINARY) $(PKG)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)

image:
	docker build \
		$(if $(GOPROXY),--build-arg GOPROXY=$(GOPROXY),) \
		-t $(IMAGE) .
