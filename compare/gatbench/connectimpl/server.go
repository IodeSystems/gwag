// Package connectimpl is the hand-written proto baseline: one
// ProjectService implementation, surfaced two ways.
//
//   - connect-go handlers — what gat's RegisterGRPC produces, but
//     against generated, type-specialised message code instead of
//     dynamicpb + reflection binding.
//   - grpc-gateway — the other "one source, several surfaces" tool in
//     Go. It transcodes REST onto the same service, which is the
//     surface gat gets from huma for free.
//
// Both routes call the same ProjectService methods, so the delta
// between them is transcoding cost, and the delta to gat is the cost
// of driving a handler through IR instead of generated code.
package connectimpl

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	gatbenchv1 "github.com/iodesystems/gwag/compare/gatbench/connectimpl/gen/gatbench/v1"
	"github.com/iodesystems/gwag/compare/gatbench/connectimpl/gen/gatbench/v1/gatbenchv1connect"
	"github.com/iodesystems/gwag/compare/gatbench/model"
)

// Service implements gatbenchv1connect.ProjectServiceHandler over the
// shared store. Stateless — the store is read-only.
type Service struct{}

func (Service) GetProject(
	ctx context.Context,
	req *connect.Request[gatbenchv1.GetProjectRequest],
) (*connect.Response[gatbenchv1.GetProjectResponse], error) {
	p, ok := model.Get(req.Msg.GetId())
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	return connect.NewResponse(&gatbenchv1.GetProjectResponse{Project: toProto(p)}), nil
}

func (Service) ListProjects(
	ctx context.Context,
	req *connect.Request[gatbenchv1.ListProjectsRequest],
) (*connect.Response[gatbenchv1.ListProjectsResponse], error) {
	src := model.List(int(req.Msg.GetLimit()))
	out := make([]*gatbenchv1.Project, len(src))
	for i, p := range src {
		out[i] = toProto(p)
	}
	return connect.NewResponse(&gatbenchv1.ListProjectsResponse{Projects: out}), nil
}

// toProto converts a store row to its proto message. gat pays an
// equivalent conversion inside its dispatcher, so this is not a
// handicap either side avoids.
func toProto(p model.Project) *gatbenchv1.Project {
	return &gatbenchv1.Project{Id: p.ID, Name: p.Name, Tags: p.Tags}
}

// NewConnect returns a mux serving the connect-go handlers. The
// generated path prefix matches the Connect / gRPC convention
// (`/gatbench.v1.ProjectService/GetProject`), same shape gat mounts.
func NewConnect() *http.ServeMux {
	mux := http.NewServeMux()
	path, handler := gatbenchv1connect.NewProjectServiceHandler(Service{})
	mux.Handle(path, handler)
	return mux
}

// Procedures the connect benchmarks POST to.
const (
	GetProcedure  = gatbenchv1connect.ProjectServiceGetProjectProcedure
	ListProcedure = gatbenchv1connect.ProjectServiceListProjectsProcedure
)
