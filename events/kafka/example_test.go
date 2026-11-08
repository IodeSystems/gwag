package kafka_test

import (
	"crypto/tls"
	"log"
	"os"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/scram"

	"github.com/iodesystems/gwag/v2/events"
	"github.com/iodesystems/gwag/v2/events/kafka"
	sr "github.com/iodesystems/gwag/v2/events/schemaregistry"
)

// A broker with SASL/SCRAM over TLS, topics named by the platform team,
// Schema Registry framing.
func Example_saslTLS() {
	backend, err := kafka.New([]string{"broker-1.corp:9093", "broker-2.corp:9093"},
		kafka.WithClientOpts(
			kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}),
			kgo.SASL(scram.Auth{
				User: os.Getenv("KAFKA_USER"),
				Pass: os.Getenv("KAFKA_PASSWORD"),
			}.AsSha512Mechanism()),
		),
		kafka.WithReplication(3),
		kafka.WithTopics(kafka.TopicTable(map[string]kafka.Topic{
			"orders.v1.OrderEvents": {
				Name:  "orders.lifecycle.v1",
				Group: func(g string) string { return "shop-" + g },
			},
		})),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Close()
	registry, err := sr.NewClient(sr.Config{
		URL:      "https://schema-registry.corp",
		Username: os.Getenv("SR_KEY"),
		Password: os.Getenv("SR_SECRET"),
	})
	if err != nil {
		log.Fatal(err)
	}
	core := events.NewCore(backend, events.WithSerde(sr.NewSerde(registry, sr.AutoRegister())))
	_ = core // ordersv1.NewBus(backend, events.WithSerde(...)) in a real service
}
