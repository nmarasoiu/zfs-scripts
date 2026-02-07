.PHONY: all generate build clean

all: build

generate:
	go generate ./...

build: generate
	go build -o usb-queue-monitor-v2 .

clean:
	rm -f usb-queue-monitor-v2 bpf_bpfel.go bpf_bpfel.o
