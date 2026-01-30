.PHONY: all generate build clean

all: build

generate:
	go generate ./...

build: generate
	go build -o blk-latency .

clean:
	rm -f blk-latency bpf_bpfel.go bpf_bpfel.o
