module github.com/iodesystems/gwag/v2/events/kafka

go 1.27.0

require (
	github.com/iodesystems/gwag/v2 v2.4.0
	github.com/twmb/franz-go v1.22.3
	github.com/twmb/franz-go/pkg/kadm v1.20.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/bufbuild/protocompile v0.14.1 // indirect
	github.com/klauspost/compress v1.20.4 // indirect
	github.com/pierrec/lz4/v4 v4.1.33 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.15.1 // indirect
	golang.org/x/sync v0.25.0 // indirect
)

replace github.com/iodesystems/gwag/v2 => ../..
