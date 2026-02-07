.PHONY: all generate build clean

all: build

generate:
	go generate ./...

build: generate
	go build -o usb-queue-monitor-during-io .

clean:
	rm -f usb-queue-monitor-during-io bpf_bpfel.go bpf_bpfel.o
