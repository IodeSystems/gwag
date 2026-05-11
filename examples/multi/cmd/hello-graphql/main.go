// hello-graphql: a tiny native GraphQL service self-registering with
// the gateway via the GraphQL stitching path. Sibling of hello-proto +
// hello-openapi; the gateway introspects this endpoint at register
// time and forwards `hello(name: ...)` queries back to it.
//
// Exists primarily so bench traffic graphql --direct has a format-
// native upstream to compare against. The gateway's `gateway.AddGraphQL`
// path is the same one external GraphQL services take in production;
// this binary is just a tiny stand-in.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/handler"

	"github.com/iodesystems/go-api-gateway/gw/controlclient"
)

func newSchema() (graphql.Schema, error) {
	helloPayload := graphql.NewObject(graphql.ObjectConfig{
		Name:        "HelloPayload",
		Description: "Hello reply object.",
		Fields: graphql.Fields{
			"greeting": &graphql.Field{
				Type:        graphql.NewNonNull(graphql.String),
				Description: "Greeting line, e.g. \"Hello, alice!\".",
			},
		},
	})
	root := graphql.NewObject(graphql.ObjectConfig{
		Name:        "Query",
		Description: "Format-native GraphQL sibling of hello-proto and hello-openapi; the direct-dial baseline for `bench traffic graphql --direct`.",
		Fields: graphql.Fields{
			"hello": &graphql.Field{
				Type:        graphql.NewNonNull(helloPayload),
				Description: "Greet the named recipient.",
				Args: graphql.FieldConfigArgument{
					"name": &graphql.ArgumentConfig{
						Type:        graphql.NewNonNull(graphql.String),
						Description: "Recipient name; echoed back in the greeting.",
					},
				},
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					name, _ := p.Args["name"].(string)
					return map[string]any{"greeting": "Hello, " + name + "!"}, nil
				},
			},
		},
	})
	return graphql.NewSchema(graphql.SchemaConfig{Query: root})
}

func main() {
	addr := flag.String("addr", ":50054", "HTTP listen address")
	gatewayAddr := flag.String("gateway", "localhost:50090", "Gateway control plane address")
	advertise := flag.String("advertise", "", "GraphQL endpoint URL to advertise to the gateway (e.g. http://localhost:50054/graphql). Defaults to http://<addr>/graphql.")
	namespace := flag.String("namespace", "hello_graphql", "Namespace to register under")
	version := flag.String("version", "v1", "Service version (unstable / vN)")
	path := flag.String("path", "/graphql", "HTTP path on which to serve GraphQL")
	flag.Parse()

	if *advertise == "" {
		host := *addr
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		*advertise = "http://" + host + *path
	}

	schema, err := newSchema()
	if err != nil {
		log.Fatalf("build schema: %v", err)
	}
	h := handler.New(&handler.Config{
		Schema:     &schema,
		Pretty:     false,
		GraphiQL:   false,
		Playground: false,
	})
	mux := http.NewServeMux()
	mux.Handle(*path, h)

	srv := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		log.Printf("hello-graphql listening on %s%s", *addr, *path)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	reg, err := controlclient.SelfRegister(context.Background(), controlclient.Options{
		GatewayAddr: *gatewayAddr,
		// ServiceAddr is ignored for GraphQL bindings — the endpoint URL
		// is the dispatch destination too — but Options requires a non-
		// empty value, so feed it the same URL.
		ServiceAddr: *advertise,
		InstanceID:  fmt.Sprintf("hello-graphql@%s", *addr),
		Services: []controlclient.Service{{
			Namespace:       *namespace,
			Version:         *version,
			GraphQLEndpoint: *advertise,
		}},
	})
	if err != nil {
		log.Fatalf("self-register: %v", err)
	}
	log.Printf("hello-graphql registered with %s as %s:%s (endpoint %s)", *gatewayAddr, *namespace, *version, *advertise)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("hello-graphql shutting down")
	_ = reg.Close(context.Background())
	_ = srv.Shutdown(context.Background())
}
