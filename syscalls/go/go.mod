module syscall-latency

go 1.21

require (
	github.com/DataDog/sketches-go v1.4.4
	github.com/cilium/ebpf v0.12.3
	github.com/hashicorp/golang-lru/v2 v2.0.7
	github.com/nmarasoiu/zfs-scripts/ringpoll v0.0.0
	golang.org/x/sys v0.14.1-0.20231108175955-e4099bfacb8c
)

replace github.com/nmarasoiu/zfs-scripts/ringpoll => ../../ringpoll

require (
	golang.org/x/exp v0.0.0-20230224173230-c95f2b4c22f2 // indirect
	google.golang.org/protobuf v1.28.0 // indirect
)
