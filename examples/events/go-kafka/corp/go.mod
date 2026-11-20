module github.com/iodesystems/gwag/v2/examples/events/go-kafka/corp

go 1.26.2

require (
	github.com/iodesystems/gwag/v2 v2.4.0
	github.com/iodesystems/gwag/v2/events/kafka v0.0.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/bufbuild/protocompile v0.14.1 // indirect
	github.com/klauspost/compress v1.20.3 // indirect
	github.com/pierrec/lz4/v4 v4.1.33 // indirect
	github.com/twmb/franz-go v1.22.3 // indirect
	github.com/twmb/franz-go/pkg/kadm v1.20.0 // indirect
	github.com/twmb/franz-go/pkg/kmsg v1.15.1 // indirect
	golang.org/x/crypto v0.58.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20261118221215-c8830f628b3f // indirect
	google.golang.org/grpc v1.85.0 // indirect
)

replace github.com/iodesystems/gwag/v2 => ../../../..

replace github.com/iodesystems/gwag/v2/events/kafka => ../../../../events/kafka
