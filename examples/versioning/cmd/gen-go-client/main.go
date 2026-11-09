// Command gen-go-client generates Go message types for one orders
// version from its frozen proto snapshot (api/orders/vN/proto.json),
// with protoc-gen-go's generator at the version go.mod pins. No protoc
// and nothing from PATH: the snapshot is already a compiled
// FileDescriptorProto.
//
//	go run ./examples/versioning/cmd/gen-go-client -snapshot api/orders/v1/proto.json -pkg ordersv1 -out clients/go
//
// Calls go through connect.NewClient with these messages (see
// cmd/old-client); connect needs no generated service stubs.
package main

import (
	"flag"
	"log"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/cmd/protoc-gen-go/internal_gengo"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

const module = "github.com/iodesystems/gwag/v2/examples/versioning/clients/go"

func main() {
	snap := flag.String("snapshot", "", "proto snapshot (gwag schema snapshot canonical JSON)")
	pkg := flag.String("pkg", "", "Go package name, also the directory under -out")
	out := flag.String("out", "", "output directory (module root of the clients)")
	flag.Parse()
	if *snap == "" || *pkg == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	b, err := os.ReadFile(*snap)
	if err != nil {
		log.Fatal(err)
	}
	fdp := &descriptorpb.FileDescriptorProto{}
	if err := protojson.Unmarshal(b, fdp); err != nil {
		log.Fatal(err)
	}
	if fdp.Options == nil {
		fdp.Options = &descriptorpb.FileOptions{}
	}
	fdp.Options.GoPackage = proto.String(module + "/" + *pkg + ";" + *pkg)

	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{fdp.GetName()},
		Parameter:      proto.String("module=" + module),
		ProtoFile:      []*descriptorpb.FileDescriptorProto{fdp},
	}
	plugin, err := protogen.Options{}.New(req)
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range plugin.Files {
		if f.Generate {
			internal_gengo.GenerateFile(plugin, f)
		}
	}
	resp := plugin.Response()
	if resp.Error != nil {
		log.Fatal(resp.GetError())
	}
	for _, f := range resp.File {
		p := filepath.Join(*out, f.GetName())
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.GetContent()), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
