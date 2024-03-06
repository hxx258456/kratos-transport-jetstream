package transport

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/go-kratos/kratos/v2/log"

	"github.com/stretchr/testify/assert"
	api "github.com/tx7do/kratos-transport/testing/api/manual"

	jetBroker "github.com/hxx258456/kratos-transport-jetstream/broker"
	natsGo "github.com/nats-io/nats.go"
	"github.com/tx7do/kratos-transport/broker"
	"github.com/tx7do/kratos-transport/broker/nats"
	protoApi "github.com/tx7do/kratos-transport/testing/api/protobuf"
)

const (
	localBroker = "nats://127.0.0.1:4222"
	testTopic   = "test_topic"
)

func handleHygrothermograph(_ context.Context, topic string, headers broker.Headers, msg *protoApi.Hygrothermograph) error {
	log.Infof("Topic %s, Headers: %+v, Payload: %+v\n", topic, headers, msg)
	return nil
}

func TestServer(t *testing.T) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGKILL, syscall.SIGTERM, syscall.SIGQUIT)

	ctx := context.Background()

	srv := NewServer(
		WithAddress([]string{localBroker}),
		WithCodec("json"),
		WithBrokerOptions(jetBroker.WithJetStream(natsGo.StreamConfig{
			Name:      "stream-1",
			Subjects:  []string{"stream.*"},
			Retention: natsGo.WorkQueuePolicy,
		})),
	)

	err := RegisterSubscriber(srv,
		"stream.1",
		handleHygrothermograph,
		broker.WithQueueName("stream-1-group"),
		jetBroker.WithDeliverAll(),
	)
	assert.Nil(t, err)

	err = RegisterSubscriber(srv,
		"stream.2",
		handleHygrothermograph,
		broker.WithQueueName("stream-2-group"),
		jetBroker.WithDeliverAll(),
	)
	assert.Nil(t, err)

	if err := srv.Start(ctx); err != nil {
		panic(err)
	}

	defer func() {
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("expected nil got %v", err)
		}
	}()

	<-interrupt
}

func TestClient(t *testing.T) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	ctx := context.Background()

	b := nats.NewBroker(
		broker.WithAddress(localBroker),
		broker.WithCodec("json"),
	)

	_ = b.Init()

	if err := b.Connect(); err != nil {
		t.Logf("cant connect to broker, skip: %v", err)
		t.Skip()
	}

	var msg api.Hygrothermograph
	const count = 10
	for i := 0; i < count; i++ {
		startTime := time.Now()
		msg.Humidity = float64(rand.Intn(100))
		msg.Temperature = float64(rand.Intn(100))
		err := b.Publish(ctx, testTopic, msg)
		assert.Nil(t, err)
		elapsedTime := time.Since(startTime) / time.Millisecond
		fmt.Printf("Publish %d, elapsed time: %dms, Humidity: %.2f Temperature: %.2f\n",
			i, elapsedTime, msg.Humidity, msg.Temperature)
	}

	fmt.Printf("total send %d messages\n", count)

	<-interrupt
}
