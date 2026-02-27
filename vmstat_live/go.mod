module vmstat_live

go 1.21

require (
	github.com/DataDog/sketches-go v1.4.4
	psiparse v0.0.0
)

require google.golang.org/protobuf v1.28.0 // indirect

replace psiparse => ../psiparse
