package gateway

import (
	"fmt"
	"io"
	"sync"

	"github.com/IodeSystems/graphql-go"
	"github.com/IodeSystems/graphql-go/language/ast"
)

// Upload is the Go-side value of a GraphQL Upload scalar. It carries
// the metadata + body of a multipart file part the gateway received
// via the graphql-multipart-request-spec wire format.
//
// The File reader streams the part body; the caller (a dispatcher
// forwarding the upload upstream) is responsible for closing it when
// finished. Size is the wire-reported byte count; -1 means unknown.
//
// Stability: stable
type Upload struct {
	Filename    string
	ContentType string
	Size        int64
	File        io.ReadCloser
}

// uploadScalar is built once per process — graphql-go rejects two
// scalars with the same Name in a single Schema, so every schema
// build references this shared instance.
var (
	uploadScalarOnce sync.Once
	uploadScalarInst *graphql.Scalar
)

// UploadScalar returns the shared `Upload` GraphQL scalar. Mutations
// that accept files declare a variable of type `Upload!`; the
// multipart-request-spec parser substitutes a *Upload value at the
// corresponding path before execution.
//
// Stability: stable
func UploadScalar() *graphql.Scalar {
	uploadScalarOnce.Do(func() {
		uploadScalarInst = graphql.NewScalar(graphql.ScalarConfig{
			Name: "Upload",
			Description: "The `Upload` scalar represents a file streamed in via " +
				"the graphql-multipart-request-spec wire format " +
				"(https://github.com/jaydenseric/graphql-multipart-request-spec). " +
				"Input-only: serialization back to JSON returns null.",
			Serialize: func(any) any {
				// Output position is undefined for Upload — clients that
				// echo an Upload back in a response payload get null.
				return nil
			},
			ParseValue: func(v any) any {
				// Variable coercion path: the multipart parser already
				// substituted *Upload values at the mapped variable
				// paths, so ParseValue is a pass-through identity for
				// the recognised type. Anything else is a hard error so
				// a misconfigured client doesn't silently drop the file.
				if v == nil {
					return nil
				}
				if u, ok := v.(*Upload); ok {
					return u
				}
				return fmt.Errorf("Upload: expected *Upload from multipart parser, got %T", v)
			},
			ParseLiteral: func(ast.Value) any {
				// Upload cannot appear as a literal in a query document;
				// it must come through variables paired with multipart
				// file parts. Returning nil makes graphql-go report a
				// type-coercion failure for `mutation { upload(file: …)}`.
				return nil
			},
		})
	})
	return uploadScalarInst
}
