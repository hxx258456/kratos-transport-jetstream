package broker

import (
	"context"
	"errors"
	"strings"
	"sync"

	kProto "github.com/go-kratos/kratos/v2/encoding/proto"
	"google.golang.org/protobuf/proto"

	"github.com/go-kratos/kratos/v2/log"
	natsGo "github.com/nats-io/nats.go"

	"github.com/tx7do/kratos-transport/broker"
	"github.com/tx7do/kratos-transport/tracing"

	"go.opentelemetry.io/otel/attribute"
	semConv "go.opentelemetry.io/otel/semconv/v1.12.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	defaultAddr = "nats://127.0.0.1:5222"
)

type natsBroker struct {
	sync.Once
	sync.RWMutex

	connected bool

	options broker.Options

	conn        *natsGo.Conn
	natsOpts    natsGo.Options
	jsCtx       natsGo.JetStreamContext
	streamInfo  *natsGo.StreamInfo
	subscribers *broker.SubscriberSyncMap

	drain     bool
	closeCh   chan error
	streamCfg natsGo.StreamConfig

	producerTracer *tracing.Tracer
	consumerTracer *tracing.Tracer
}

func NewBroker(opts ...broker.Option) broker.Broker {
	options := broker.NewOptionsAndApply(opts...)

	b := &natsBroker{
		options:     options,
		subscribers: broker.NewSubscriberSyncMap(),
	}

	return b
}

func (b *natsBroker) Address() string {
	if b.conn != nil && b.conn.IsConnected() {
		return b.conn.ConnectedUrl()
	}

	if len(b.options.Addrs) > 0 {
		return b.options.Addrs[0]
	}

	return defaultAddr
}

func (b *natsBroker) Name() string {
	return "NATS"
}

func (b *natsBroker) Options() broker.Options {
	return b.options
}

func (b *natsBroker) Init(opts ...broker.Option) error {
	b.setOption(opts...)

	if len(b.options.Tracings) > 0 {
		b.producerTracer = tracing.NewTracer(trace.SpanKindProducer, "nats-producer", b.options.Tracings...)
		b.consumerTracer = tracing.NewTracer(trace.SpanKindConsumer, "nats-consumer", b.options.Tracings...)
	}

	return nil
}

func (b *natsBroker) setAddrs(addrs []string) []string {
	//nolint:prealloc
	var cAddrs []string
	for _, addr := range addrs {
		if len(addr) == 0 {
			continue
		}
		if !strings.HasPrefix(addr, "nats://") {
			addr = "nats://" + addr
		}
		cAddrs = append(cAddrs, addr)
	}
	if len(cAddrs) == 0 {
		cAddrs = []string{natsGo.DefaultURL}
	}
	return cAddrs
}

func (b *natsBroker) setOption(opts ...broker.Option) {
	for _, o := range opts {
		o(&b.options)
	}

	b.Once.Do(func() {
		b.natsOpts = natsGo.GetDefaultOptions()
	})

	if value, ok := b.options.Context.Value(optionsKey{}).(natsGo.Options); ok {
		b.natsOpts = value
	}

	if len(b.options.Addrs) == 0 {
		b.options.Addrs = b.natsOpts.Servers
	}

	if !b.options.Secure {
		b.options.Secure = b.natsOpts.Secure
	}

	if b.options.TLSConfig == nil {
		b.options.TLSConfig = b.natsOpts.TLSConfig
	}
	b.setAddrs(b.options.Addrs)

	if b.options.Context.Value(drainConnectionKey{}) != nil {
		b.drain = true
		b.closeCh = make(chan error)
		b.natsOpts.ClosedCB = b.onClose
		b.natsOpts.AsyncErrorCB = b.onAsyncError
		b.natsOpts.DisconnectedErrCB = b.onDisconnectedError
	}

	if b.options.Context.Value(jetstreamConfKey{}) != nil {
		jetStreamCnf, ok := b.options.Context.Value(jetstreamConfKey{}).(natsGo.StreamConfig)
		if ok {
			b.streamCfg = jetStreamCnf
		}
	}
}

func (b *natsBroker) Connect() error {
	b.Lock()
	defer b.Unlock()

	if b.connected {
		return nil
	}

	status := natsGo.CLOSED
	if b.conn != nil {
		status = b.conn.Status()
	}

	switch status {
	case natsGo.CONNECTED, natsGo.RECONNECTING, natsGo.CONNECTING:
		b.connected = true
		return nil
	default: // DISCONNECTED or CLOSED or DRAINING
		opts := b.natsOpts
		opts.Servers = b.options.Addrs
		opts.Secure = b.options.Secure
		opts.TLSConfig = b.options.TLSConfig

		if b.options.TLSConfig != nil {
			opts.Secure = true
		}

		c, err := opts.Connect()
		if err != nil {
			return err
		}
		if b.streamCfg.Name != "" {
			b.jsCtx, err = c.JetStream()
			if err != nil {
				return err
			}
			var s *natsGo.StreamInfo
			s, err = b.jsCtx.StreamInfo(b.streamCfg.Name)
			if err != nil {
				if errors.Is(err, natsGo.ErrStreamNotFound) {
					s, err = b.jsCtx.AddStream(&b.streamCfg)
					if err != nil {
						return err
					}
				} else {
					return err
				}
			}
			s, err = b.jsCtx.UpdateStream(&b.streamCfg)
			if err != nil {
				return err
			}
			b.streamInfo = s

		}
		b.conn = c
		b.connected = true
		return nil
	}
}

func (b *natsBroker) Disconnect() error {
	b.Lock()
	defer b.Unlock()

	if b.drain {
		if b.conn != nil {
			_ = b.conn.Drain()
		}
		b.closeCh <- nil
	}

	b.subscribers.Clear()

	if b.conn != nil {
		b.conn.Close()
	}

	b.connected = false

	return nil
}

func (b *natsBroker) Publish(ctx context.Context, topic string, msg broker.Any, opts ...broker.PublishOption) error {
	buf, err := broker.Marshal(b.options.Codec, msg)
	if err != nil {
		return err
	}

	return b.publish(ctx, topic, buf, opts...)
}

func (b *natsBroker) publish(ctx context.Context, topic string, buf []byte, opts ...broker.PublishOption) error {
	b.RLock()
	defer b.RUnlock()

	if b.conn == nil {
		return errors.New("not connected")
	}

	options := broker.PublishOptions{
		Context: ctx,
	}
	for _, o := range opts {
		o(&options)
	}

	m := natsGo.NewMsg(topic)

	m.Data = buf

	if headers, ok := options.Context.Value(headersKey{}).(map[string][]string); ok {
		for k, v := range headers {
			for _, vv := range v {
				m.Header.Add(k, vv)
			}
		}
	}

	span := b.startProducerSpan(options.Context, m)
	var err error
	if b.streamInfo != nil {
		_, err = b.jsCtx.PublishMsg(m)
	} else {
		err = b.conn.PublishMsg(m)
	}

	b.finishProducerSpan(span, err)

	return err
}

func (b *natsBroker) Subscribe(topic string, handler broker.Handler, binder broker.Binder, opts ...broker.SubscribeOption) (broker.Subscriber, error) {
	b.RLock()
	if b.conn == nil {
		b.RUnlock()
		return nil, errors.New("not connected")
	}
	b.RUnlock()

	options := broker.SubscribeOptions{
		Context: context.Background(),
		AutoAck: true,
	}
	for _, o := range opts {
		o(&options)
	}

	subs := &subscriber{
		n:       b,
		s:       nil,
		options: options,
	}

	fn := func(msg *natsGo.Msg) {
		m := &broker.Message{
			Headers: natsHeaderToMap(msg.Header),
			Body:    nil,
		}

		pub := &publication{t: msg.Subject, m: m}

		ctx, span := b.startConsumerSpan(options.Context, msg)

		eh := b.options.ErrorHandler

		if binder != nil {
			if b.options.Codec.Name() == kProto.Name {
				m.Body = binder().(proto.Message)
			} else {
				m.Body = binder()
			}
		} else {
			m.Body = msg.Data
		}

		if err := broker.Unmarshal(b.options.Codec, msg.Data, &m.Body); err != nil {
			pub.err = err
			log.Errorf("[nats]: unmarshal message failed: %v", err)
			if eh != nil {
				_ = eh(b.options.Context, pub)
			}
			return
		}

		if err := handler(ctx, pub); err != nil {
			pub.err = err
			log.Errorf("[nats]: process message failed: %v", err)
			if eh != nil {
				_ = eh(b.options.Context, pub)
			}
		}
		if options.AutoAck {
			if err := msg.Ack(); err != nil {
				log.Errorf("[nats]: unable to commit msg: %v", err)
			}
		}

		b.finishConsumerSpan(span)
	}

	var sub *natsGo.Subscription
	var err error

	b.RLock()
	if b.streamInfo != nil {
		subOpts := []natsGo.SubOpt{}
		if options.Context.Value(deliverAllKey{}) != nil {
			subOpts = append(subOpts, natsGo.DeliverAll())
		}

		if options.Context.Value(deliverNewKey{}) != nil {
			subOpts = append(subOpts, natsGo.DeliverNew())
		}

		if options.Context.Value(deliverLastKey{}) != nil {
			subOpts = append(subOpts, natsGo.DeliverLast())
		}
		if len(options.Queue) > 0 {

			sub, err = b.jsCtx.QueueSubscribe(topic, options.Queue, fn, subOpts...)

		} else {
			sub, err = b.jsCtx.Subscribe(topic, fn)
		}

	} else {
		if len(options.Queue) > 0 {
			sub, err = b.conn.QueueSubscribe(topic, options.Queue, fn)
		} else {
			sub, err = b.conn.Subscribe(topic, fn)
		}
	}

	b.RUnlock()
	if err != nil {
		return nil, err
	}

	subs.s = sub

	b.subscribers.Add(topic, subs)

	return subs, nil
}

func (b *natsBroker) onClose(_ *natsGo.Conn) {
	b.closeCh <- nil
}

func (b *natsBroker) onAsyncError(_ *natsGo.Conn, _ *natsGo.Subscription, err error) {
	if errors.Is(err, natsGo.ErrDrainTimeout) {
		b.closeCh <- err
	}
}

func (b *natsBroker) onDisconnectedError(_ *natsGo.Conn, err error) {
	b.closeCh <- err
}

func (b *natsBroker) startProducerSpan(ctx context.Context, msg *natsGo.Msg) trace.Span {
	if b.producerTracer == nil {
		return nil
	}

	carrier := NewMessageCarrier(msg)

	attrs := []attribute.KeyValue{
		semConv.MessagingSystemKey.String("nats"),
		semConv.MessagingDestinationKindTopic,
		semConv.MessagingDestinationKey.String(msg.Subject),
	}

	var span trace.Span
	ctx, span = b.producerTracer.Start(ctx, carrier, attrs...)

	return span
}

func (b *natsBroker) finishProducerSpan(span trace.Span, err error) {
	if b.producerTracer == nil {
		return
	}

	b.producerTracer.End(context.Background(), span, err)
}

func (b *natsBroker) startConsumerSpan(ctx context.Context, msg *natsGo.Msg) (context.Context, trace.Span) {
	if b.consumerTracer == nil {
		return ctx, nil
	}

	carrier := NewMessageCarrier(msg)

	attrs := []attribute.KeyValue{
		semConv.MessagingSystemKey.String("nats"),
		semConv.MessagingDestinationKindTopic,
		semConv.MessagingDestinationKey.String(msg.Subject),
		semConv.MessagingOperationReceive,
	}

	var span trace.Span
	ctx, span = b.consumerTracer.Start(ctx, carrier, attrs...)

	return ctx, span
}

func (b *natsBroker) finishConsumerSpan(span trace.Span) {
	if b.consumerTracer == nil {
		return
	}

	b.consumerTracer.End(context.Background(), span, nil)
}
