.PHONY: all generate build clean

all: build

generate:
	go generate ./...

build: generate
	go build -o blk-ddsketch .

clean:
	rm -f blk-ddsketch bpf_bpfel.go bpf_bpfel.o
