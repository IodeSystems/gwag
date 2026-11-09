// Command old-client is a client built against orders v1 that nobody
// will rebuild soon. It calls the gateway's gRPC ingress (the
// control-plane port of `gwag up`) with the v1 messages generated from
// the frozen v1 snapshot (clients/go/ordersv1). conn.Invoke needs no
// generated service stubs.
//
//	go run ./examples/versioning/cmd/old-client -grpc localhost:50090
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	ordersv1 "github.com/iodesystems/gwag/v2/examples/versioning/clients/go/ordersv1"
)

func main() {
	addr := flag.String("grpc", "localhost:50090", "gateway gRPC ingress address")
	caller := flag.String("caller", "old-go-client", "caller-id metadata sent on every call")
	flag.Parse()
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "caller-id", *caller)

	o := &ordersv1.Order{}
	if err := conn.Invoke(ctx, "/orders.v1.Service/getOrder", &ordersv1.GetOrderRequest{Id: "o-1"}, o); err != nil {
		log.Fatalf("v1 getOrder: %v", err)
	}
	fmt.Printf("v1 getOrder o-1: amount=%.2f customerName=%q\n", o.GetAmount(), o.GetCustomerName())

	l := &ordersv1.ListOutBody{}
	if err := conn.Invoke(ctx, "/orders.v1.Service/listOrders", &ordersv1.ListOrdersRequest{}, l); err != nil {
		log.Fatalf("v1 listOrders: %v", err)
	}
	fmt.Printf("v1 listOrders: %d orders (USD only)\n", len(l.GetOrders()))
}
