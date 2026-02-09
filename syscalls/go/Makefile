.PHONY: all generate build clean test vet fmt lint vuln tidy check

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
LDFLAGS  = -X main.version=$(VERSION) -X main.commit=$(COMMIT)

all: build

generate:
	go generate ./...

build: generate
	go build -ldflags "$(LDFLAGS)" -o syscall-latency .

# --- quality targets ---

test:
	go test -count=1 ./...

test-race:
	go test -count=1 -race ./...

test-cover:
	go test -count=1 -coverprofile=cover.out ./...
	go tool cover -func=cover.out | tail -1
	@rm -f cover.out

vet:
	go vet ./...

fmt:
	@bad=$$(gofmt -l .); \
	if [ -n "$$bad" ]; then \
		echo "gofmt needed on:"; echo "$$bad"; exit 1; \
	fi

lint:
	staticcheck ./...

vuln:
	govulncheck ./...

tidy:
	@cp go.mod go.mod.check && cp go.sum go.sum.check
	go mod tidy
	@if ! diff -q go.mod go.mod.check >/dev/null 2>&1 || ! diff -q go.sum go.sum.check >/dev/null 2>&1; then \
		mv go.mod.check go.mod; mv go.sum.check go.sum; \
		echo "go.mod/go.sum not tidy — run 'go mod tidy'"; exit 1; \
	fi
	@rm -f go.mod.check go.sum.check

# Run all checks (the one target to rule them all)
check: fmt vet lint test-race vuln tidy
	@echo "all checks passed"

clean:
	rm -f syscall-latency cover.out bpf_bpfel.go bpf_bpfel.o
