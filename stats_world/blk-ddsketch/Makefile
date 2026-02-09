.PHONY: all generate build clean test vet fmt

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS  = -X main.version=$(VERSION) -X main.commit=$(COMMIT)

all: build

generate:
	go generate ./...

build: generate
	go build -ldflags "$(LDFLAGS)" -o blk-ddsketch .

test:
	go test -v ./...

vet:
	go vet ./...

fmt:
	gofmt -l .

clean:
	rm -f blk-ddsketch bpf_bpfel.go bpf_bpfel.o
