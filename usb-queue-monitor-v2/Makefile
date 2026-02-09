.PHONY: all build clean test vet fmt

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS  = -X main.version=$(VERSION) -X main.commit=$(COMMIT)

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o usb-queue-monitor-v2 .

test:
	go test -v ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

clean:
	rm -f usb-queue-monitor-v2
