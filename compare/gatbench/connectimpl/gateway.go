package connectimpl

import (
	"context"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gatbenchv1 "github.com/iodesystems/gwag/compare/gatbench/connectimpl/gen/gatbench/v1"
	"github.com/iodesystems/gwag/compare/gatbench/model"
)

// grpcService implements the grpc-go server interface grpc-gateway
// transcodes onto. Same store, same conversion as the connect
// implementation — the two differ only in the generated wrapper each
// framework expects.
type grpcService struct {
	gatbenchv1.UnimplementedProjectServiceServer
}

func (grpcService) GetProject(ctx context.Context, req *gatbenchv1.GetProjectRequest) (*gatbenchv1.GetProjectResponse, error) {
	p, ok := model.Get(req.GetId())
	if !ok {
		return nil, status.Error(codes.NotFound, "no such project")
	}
	return &gatbenchv1.GetProjectResponse{Project: toProto(p)}, nil
}

func (grpcService) ListProjects(ctx context.Context, req *gatbenchv1.ListProjectsRequest) (*gatbenchv1.ListProjectsResponse, error) {
	src := model.List(int(req.GetLimit()))
	out := make([]*gatbenchv1.Project, len(src))
	for i, p := range src {
		out[i] = toProto(p)
	}
	return &gatbenchv1.ListProjectsResponse{Projects: out}, nil
}

// NewGRPCGateway returns grpc-gateway's REST mux over the same
// service.
//
// RegisterProjectServiceHandlerServer is the in-process registration:
// the transcoder calls the server's methods directly instead of
// dialling a loopback gRPC connection. That is the fair comparison
// against gat, which also dispatches in-process; the
// FromEndpoint variant would add a full network round trip and
// measure the loopback, not the framework.
func NewGRPCGateway() (http.Handler, error) {
	mux := runtime.NewServeMux()
	if err := gatbenchv1.RegisterProjectServiceHandlerServer(context.Background(), mux, grpcService{}); err != nil {
		return nil, err
	}
	return mux, nil
}
