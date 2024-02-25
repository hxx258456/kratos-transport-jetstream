package broker

import (
	natsGo "github.com/nats-io/nats.go"
	"github.com/tx7do/kratos-transport/broker"
)

type optionsKey struct{}
type drainConnectionKey struct{}

func Options(opts natsGo.Options) broker.Option {
	return broker.OptionContextWithValue(optionsKey{}, opts)
}

func DrainConnection() broker.Option {
	return broker.OptionContextWithValue(drainConnectionKey{}, struct{}{})
}

type jetstreamConfKey struct{}

func WithJetStream(streamCnf natsGo.StreamConfig) broker.Option {
	return broker.OptionContextWithValue(jetstreamConfKey{}, streamCnf)
}

///////////////////////////////////////////////////////////////////////////////

type headersKey struct{}

func WithHeaders(h map[string][]string) broker.PublishOption {
	return broker.PublishContextWithValue(headersKey{}, h)
}

///////////////////////////////////////////////////////////////////////////////

type deliverAllKey struct{}

func WithDeliverAll() broker.SubscribeOption {
	return broker.SubscribeContextWithValue(deliverAllKey{}, struct{}{})
}

type deliverNewKey struct{}

func WithDeliverNew() broker.SubscribeOption {
	return broker.SubscribeContextWithValue(deliverNewKey{}, struct{}{})
}

type deliverLastKey struct{}

func WithDeliverLast() broker.SubscribeOption {
	return broker.SubscribeContextWithValue(deliverLastKey{}, struct{}{})
}
