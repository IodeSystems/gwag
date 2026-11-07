// Go client for services behind a gwag gateway, built from the
// gateway's exported proto. Calls the same RPCs over the gRPC ingress
// and over Connect.
//
//	./gen.sh                      # writes gen/
//	go run ./examples/clients/go-proto
//
// Env: GWAG_GRPC (default localhost:50090, the control-plane port),
// GWAG_CONNECT (default http://localhost:8080/api/connect).
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	greeterv1 "github.com/iodesystems/gwag/v2/examples/clients/go-proto/gen/greeter/v1"
	libraryv1 "github.com/iodesystems/gwag/v2/examples/clients/go-proto/gen/library/v1"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// gRPC ingress. In examples/multi it shares the control-plane port.
	conn, err := grpc.NewClient(env("GWAG_GRPC", "localhost:50090"),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	greeter := greeterv1.NewGreeterServiceClient(conn)
	hello, err := greeter.Hello(ctx, &greeterv1.HelloRequest{Name: "grpc"})
	if err != nil {
		log.Fatalf("grpc Hello: %v", err)
	}
	fmt.Printf("grpc    greeter.Hello -> %s\n", hello.GetGreeting())

	library := libraryv1.NewLibraryServiceClient(conn)
	list, err := library.ListBooks(ctx, &libraryv1.ListBooksRequest{})
	if err != nil {
		log.Fatalf("grpc ListBooks: %v", err)
	}
	fmt.Printf("grpc    library.ListBooks -> %d books\n", len(list.GetBooks()))
	for _, b := range list.GetBooks() {
		fmt.Printf("grpc      %s  %s (%d)\n", b.GetId(), b.GetTitle(), b.GetYear())
	}

	// Connect ingress. The generated message types work as they are;
	// connect.NewClient needs a URL per method, no connect codegen.
	// WithProtoJSON sends JSON on the wire; drop it for binary proto.
	base := env("GWAG_CONNECT", "http://localhost:8080/api/connect")
	connectHello := connect.NewClient[greeterv1.HelloRequest, greeterv1.HelloResponse](
		http.DefaultClient, base+"/greeter.v1.GreeterService/Hello", connect.WithProtoJSON())
	cresp, err := connectHello.CallUnary(ctx, connect.NewRequest(&greeterv1.HelloRequest{Name: "connect"}))
	if err != nil {
		log.Fatalf("connect Hello: %v", err)
	}
	fmt.Printf("connect greeter.Hello -> %s\n", cresp.Msg.GetGreeting())

	connectGet := connect.NewClient[libraryv1.GetBookRequest, libraryv1.Book](
		http.DefaultClient, base+"/library.v1.LibraryService/GetBook", connect.WithProtoJSON())
	book, err := connectGet.CallUnary(ctx, connect.NewRequest(&libraryv1.GetBookRequest{Id: "1"}))
	if err != nil {
		log.Fatalf("connect GetBook: %v", err)
	}
	fmt.Printf("connect library.GetBook(1) -> %s by %s\n", book.Msg.GetTitle(), book.Msg.GetAuthor())
}
