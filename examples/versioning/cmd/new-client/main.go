// Command new-client is a client on orders v2, calling the same
// gateway and the same service process as old-client. Messages are
// generated from the v2 snapshot (clients/go/ordersv2).
//
//	go run ./examples/versioning/cmd/new-client -grpc localhost:50090
package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	ordersv2 "github.com/iodesystems/gwag/v2/examples/versioning/clients/go/ordersv2"
)

func main() {
	addr := flag.String("grpc", "localhost:50090", "gateway gRPC ingress address")
	caller := flag.String("caller", "new-go-client", "caller-id metadata sent on every call")
	flag.Parse()
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()
	ctx := metadata.AppendToOutgoingContext(context.Background(), "caller-id", *caller)

	o := &ordersv2.Order{}
	if err := conn.Invoke(ctx, "/orders.v2.Service/getOrder", &ordersv2.GetOrderRequest{Id: "o-3"}, o); err != nil {
		log.Fatalf("v2 getOrder: %v", err)
	}
	fmt.Printf("v2 getOrder o-3: amountCents=%d currency=%s customer=%q\n", o.GetAmountCents(), o.GetCurrency(), o.GetCustomer())

	r := &ordersv2.Order{}
	req := &ordersv2.RefundOrderRequest{Id: "o-3", Body: &ordersv2.RefundInBody{AmountCents: 200}}
	if err := conn.Invoke(ctx, "/orders.v2.Service/refundOrder", req, r); err != nil {
		log.Fatalf("v2 refundOrder: %v", err)
	}
	fmt.Printf("v2 refundOrder o-3: refundedCents=%d\n", r.GetRefundedCents())
}
