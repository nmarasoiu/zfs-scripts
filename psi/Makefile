BIN := psi

.PHONY: all check vet test build clean

all: check build

check: vet test

vet:
	go vet ./...

test:
	go test ./...

build:
	go build -o $(BIN) .

clean:
	rm -f $(BIN)
