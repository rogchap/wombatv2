package server

import (
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

//go:generate protoc --go_out=:. --go-grpc_out=:. route_guide.proto

type routeGuideServer struct {
	UnimplementedRouteGuideServer
}

// Serve stats serving a gRPC server that is used for testing
func Serve() {
	lis, err := net.Listen("tcp", "localhost:5001")
	if err != nil {
		fmt.Fprintf(os.Stderr, "server: failed to create listener: %v", err)
	}
	s := grpc.NewServer()
	RegisterRouteGuideServer(s, &routeGuideServer{})
	reflection.Register(s)
	s.Serve(lis)
}