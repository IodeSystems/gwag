module github.com/iodesystems/gwag/v2/events/kafka

go 1.26.2

require (
	github.com/iodesystems/gwag/v2 v2.4.0
	github.com/twmb/franz-go v1.21.7
	github.com/twmb/franz-go/pkg/kadm v1.18.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/pierrec/lz4/v4 v4.1.26 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.13.1 // indirect
	golang.org/x/crypto v0.58.0 // indirect
)

replace github.com/iodesystems/gwag/v2 => ../..
