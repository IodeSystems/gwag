// Command orders serves orders v1 and v2 from one process and, with
// -gateway, registers both versions with gwag.
//
//	go run ./examples/versioning/cmd/orders -listen :18610                        # serve only
//	go run ./examples/versioning/cmd/orders -listen :18610 -gateway localhost:50090 # serve and register
//	go run ./examples/versioning/cmd/orders ... -no-v1                            # the build after v1 is deleted
//
// Each version is its own registration (instance orders-v1@ADDR and
// orders-v2@ADDR), so `gwag drain orders-v1@ADDR` can take one line out
// of rotation while the other keeps serving. One registration with two
// Services also works (docs/services.md, "Two versions in one binary");
// it drains both lines together.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/iodesystems/gwag/v2/examples/versioning/internal/server"
	"github.com/iodesystems/gwag/v2/gw/controlclient"
)

func main() {
	listen := flag.String("listen", ":18610", "HTTP listen address")
	advertise := flag.String("advertise", "", "base URL the gateway dials (default http://localhost:PORT)")
	gateways := flag.String("gateway", "", "control-plane addresses, comma separated; empty: do not register")
	noV1 := flag.Bool("no-v1", false, "serve and register v2 only (v1 deleted)")
	sunset := flag.String("discount-sunset", "2027-06-30", "date (YYYY-MM-DD, UTC) from which v1 applyDiscount answers 410; empty: never")
	flag.Parse()

	opts := server.Options{NoV1: *noV1}
	if *sunset != "" {
		t, err := time.Parse(time.DateOnly, *sunset)
		if err != nil {
			log.Fatalf("-discount-sunset: %v", err)
		}
		opts.DiscountSunset = t
	}
	svc, err := server.New(opts)
	if err != nil {
		log.Fatal(err)
	}
	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	if *advertise == "" {
		_, port, _ := net.SplitHostPort(lis.Addr().String())
		*advertise = "http://localhost:" + port
	}
	srv := &http.Server{Handler: svc.Handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(lis); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	versions := []string{"v1", "v2"}
	if *noV1 {
		versions = []string{"v2"}
	}
	log.Printf("orders %s on %s", strings.Join(versions, "+"), *advertise)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var regs []*controlclient.Registration
	if *gateways != "" {
		for _, v := range versions {
			spec, err := svc.Gat.OpenAPISpec(v)
			if err != nil {
				log.Fatal(err)
			}
			reg, err := controlclient.SelfRegister(ctx, controlclient.Options{
				GatewayAddrs: strings.Split(*gateways, ","),
				ServiceAddr:  *advertise,
				InstanceID:   "orders-" + v + "@" + *advertise,
				Services: []controlclient.Service{{
					Namespace:   "orders",
					Version:     v,
					OpenAPISpec: spec,
					Owner:       &controlclient.Owner{Team: "orders", Contact: "#orders"},
					Public:      true,
				}},
			})
			if err != nil {
				log.Fatalf("register %s: %v", v, err)
			}
			log.Printf("registered orders/%s with %s", v, reg.GatewayAddr())
			regs = append(regs, reg)
		}
	}

	<-ctx.Done()
	for _, r := range regs {
		_ = r.Close(context.Background())
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shut)
}
