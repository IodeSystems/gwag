// Command specs writes the specs the orders code generates, without a
// server: it builds the service in process and reads gat's schema
// views. check.sh compares them with the committed snapshots in api/.
//
//	go run ./examples/versioning/cmd/specs -out DIR
//
// writes DIR/v1.openapi.json, DIR/v2.openapi.json (as gwag gets them
// from controlclient), DIR/v1.fds, DIR/v2.fds (proto, one
// FileDescriptorSet per version) and DIR/schema.graphql (the one SDL
// of both versions).
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/iodesystems/gwag/v2/examples/versioning/internal/server"
)

func main() {
	out := flag.String("out", "", "output directory (required)")
	flag.Parse()
	if *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*out); err != nil {
		log.Fatal(err)
	}
}

func run(out string) error {
	svc, err := server.New(server.Options{})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, v := range []string{"v1", "v2"} {
		spec, err := svc.Gat.OpenAPISpec(v)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, v+".openapi.json"), spec, 0o644); err != nil {
			return err
		}
		if err := fetch(svc.Handler, "/api/schema/proto?version="+v, filepath.Join(out, v+".fds")); err != nil {
			return err
		}
	}
	return fetch(svc.Handler, "/api/schema/graphql", filepath.Join(out, "schema.graphql"))
}

func fetch(h http.Handler, path, file string) error {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		return fmt.Errorf("GET %s: %d %s", path, rec.Code, rec.Body.String())
	}
	b, err := io.ReadAll(rec.Body)
	if err != nil {
		return err
	}
	return os.WriteFile(file, b, 0o644)
}
