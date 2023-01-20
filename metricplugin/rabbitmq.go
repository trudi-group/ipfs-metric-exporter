package metricplugin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	pool "github.com/libp2p/go-buffer-pool"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/prometheus/client_golang/prometheus"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.uber.org/zap"
)

// RabbitMQPublisher implements publishing events to RabbitMQ.
type RabbitMQPublisher struct {
	msgOut chan Event

	logger *zap.SugaredLogger

	closingLock sync.Mutex
	closed      chan struct{}
}

func newRabbitMQPublisher(monitorName string, addr string) (*RabbitMQPublisher, error) {
	logger := log.Named("rabbitmq")

	conn, ch, err := setup(addr, logger)
	if err != nil {
		return nil, err
	}
	fmt.Printf("Bitswap tracer connected to RabbitMQ at %s\n", addr)

	// Set up some plumbing
	msgChan := make(chan Event)
	packetChan := make(chan eventPackage)
	jsonChan := make(chan binaryPackage)
	gzipChan := make(chan binaryPackage)
	closedChan := make(chan struct{})

	go packagerLoop(logger, msgChan, packetChan)
	go encodeLoop(logger, packetChan, jsonChan)
	go compressLoop(logger, jsonChan, gzipChan)
	go sendLoop(logger, monitorName, addr, conn, ch, gzipChan, closedChan)

	p := &RabbitMQPublisher{
		msgOut: msgChan,
		logger: logger,
		closed: closedChan,
	}

	return p, nil
}

// ExchangeName is the name of the exchange on RabbitMQ.
const ExchangeName = "ipfs.passive_monitoring"

// Every connection should declare the topology they expect
func setup(url string, logger *zap.SugaredLogger) (*amqp.Connection, *amqp.Channel, error) {
	logger.Debugf("Attempting to connect to %s...", url)

	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, nil, err
	}

	logger.Debugf("Connected to %s, opening channel and setting up exchange...", url)
	ch, err := conn.Channel()
	if err != nil {
		return nil, nil, err
	}

	// Declare the exchange.
	err = ch.ExchangeDeclare(ExchangeName, "topic", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("exchange.declare: %v", err)
	}

	return conn, ch, nil
}

// Close closes the publisher.
// Attempting to send messages after Close was called will result in a panic.
func (p *RabbitMQPublisher) Close() {
	p.closingLock.Lock()
	select {
	case <-p.closed:
		// Already closed
		p.closingLock.Unlock()
		return
	default:
	}
	// Not closed, close now
	close(p.msgOut)
	<-p.closed
	p.closingLock.Unlock()
}

// PublishConnectionEvent creates and attempts to publish a connection event
// from the given values.
// The event may be dropped if the connection to RabbitMQ is too slow.
// This will never block.
func (p *RabbitMQPublisher) PublishConnectionEvent(ts time.Time, connected bool, conn network.Conn) {
	eventToLog := ConnectionEvent{
		Remote:              conn.RemoteMultiaddr(),
		ConnectionEventType: Connected,
	}
	if !connected {
		eventToLog.ConnectionEventType = Disconnected
	}
	rp := conn.RemotePeer()

	p.msgOut <- Event{
		Timestamp:       ts,
		Peer:            rp.String(),
		ConnectionEvent: &eventToLog,
	}
}

// PublishBitswapMessage creates and attempts to publish a Bitswap message from
// the given values.
// The message may be dropped if the connection to RabbitMQ is too slow.
// This will never block.
func (p *RabbitMQPublisher) PublishBitswapMessage(ts time.Time, peer peer.ID, msg *BitswapMessage) {
	p.msgOut <- Event{
		Timestamp:      ts,
		Peer:           peer.String(),
		BitswapMessage: msg,
	}
}

// Event is the recording of an incoming message.
type Event struct {
	// The timestamp at which the event was recorded.
	// This defines an ordering for events.
	Timestamp time.Time `json:"timestamp"`

	// Peer is a base58-encoded string representation of the peer ID.
	Peer string `json:"peer"`

	// BitswapMessage is not nil if this event is a bitswap message.
	BitswapMessage *BitswapMessage `json:"bitswap_message,omitempty"`

	// ConnectionEvent is not nil if this event is a connection event.
	ConnectionEvent *ConnectionEvent `json:"connection_event,omitempty"`
}

type eventType int

const (
	eventTypeBitswapMessages eventType = iota
	eventTypeConnectionEvents
)

type eventPackage struct {
	events      []Event
	contentType eventType
}

type binaryPackage struct {
	payload     []byte
	numEvents   int
	contentType eventType
}

// Asynchronous loop to take single messages and bunch them up into slices.
// The slices are then sent to the encoder.
// If sending is blocked due to slow encoding or later pipeline stages, some buffering is attempted.
// If sending is blocked for a long time, values will be dropped.
func packagerLoop(logger *zap.SugaredLogger, msgIn <-chan Event, packetsOut chan eventPackage) {
	defer func() {
		logger.Info("packagerLoop quitting")
		close(packetsOut)
	}()

	droppedEvents := wiretapProcessedEvents.With(prometheus.Labels{"dropped": "true"})

	// These define the minimum size of the slice before it's sent along,
	// as well as the maximum before it is dropped.
	// Larger slices will probably take load off RabbitMQ and may compress better, but add latency to the system
	// and consume some memory, although probably not much.
	// It's unclear if large slices add noticeable copying overhead, but probably not.
	minSliceSize := 10
	maxSliceSize := 1000

	msgBuffer := &eventPackage{
		events:      make([]Event, 0, minSliceSize),
		contentType: eventTypeBitswapMessages,
	}
	connEventBuffer := &eventPackage{
		events:      make([]Event, 0, minSliceSize),
		contentType: eventTypeConnectionEvents,
	}
	var currentBuffer **eventPackage
	for msg := range msgIn {
		if msg.ConnectionEvent != nil {
			logger.Debugf("packager: got event %+v, which is a connection event", msg)
			currentBuffer = &connEventBuffer
		} else {
			logger.Debugf("packager: got event %+v, which is a bitswap message", msg)
			currentBuffer = &msgBuffer
		}

		(**currentBuffer).events = append((**currentBuffer).events, msg)
		logger.Debugf("packager: got event %+v, now have %d events in buffer", msg, len((**currentBuffer).events))

		if len((**currentBuffer).events) >= minSliceSize {
			logger.Debugf("packager: %d events in buffer, attempting to flush...", len((**currentBuffer).events))
			select {
			// TODO I _really_ hope this creates a copy of the struct, otherwise what we're doing is incorrect
			case packetsOut <- **currentBuffer:
				// ok, allocate new buffer
				(**currentBuffer).events = make([]Event, 0, minSliceSize)
			default:
				// TODO what do?
				if len((**currentBuffer).events) < maxSliceSize {
					// Maybe it's fine up to some limit?
					logger.Debugf("couldn't immediately pass on buffer of %d events to RabbitMQ, aggregating more..", len((**currentBuffer).events))
				} else {
					// Ok, let's drop them, I guess...
					droppedEvents.Add(float64(len((**currentBuffer).events)))
					logger.Warnf("dropping %d events to RabbitMQ due to backpressure", len((**currentBuffer).events))
					(**currentBuffer).events = (**currentBuffer).events[:0]
				}
			}
		}
	}
}

// Asynchronous loop to encode outgoing messages.
// This takes messages and encodes them to JSON.
// The encoded messages are then pushed into encodedOut.
// For each message, a buffer is taken from the global buffer pool.
// The consumer of encodedOut should return the buffer to the pool.
func encodeLoop(logger *zap.SugaredLogger, msgIn <-chan eventPackage, encodedOut chan binaryPackage) {
	defer func() {
		logger.Debug("encodeLoop exiting")
		close(encodedOut)
	}()

	for msg := range msgIn {
		logger.Debugf("encode: encoding message %+v (%p)", msg, msg)

		buf := pool.GlobalPool.Get(1024 * 512)
		bbuf := bytes.NewBuffer(buf)
		bbuf.Reset()
		w := json.NewEncoder(bbuf)
		if err := w.Encode(msg.events); err != nil {
			panic(fmt.Sprintf("encode: unable to marshal %+v to JSON: %s", msg.events, err))
		}

		logger.Debugf("encode: encoded outgoing message: %q", bbuf.String())

		encodedOut <- binaryPackage{
			payload:     bbuf.Bytes(),
			numEvents:   len(msg.events),
			contentType: msg.contentType,
		}
	}
}

// Asynchronous loop to compress outgoing messages.
// This takes JSON-encoded messages and compresses them using gzip.
// For each message, a buffer is taken from the global buffer pool.
// The buffer for the incoming, JSON-encoded bytes is returned to the global
// pool after use.
// The consumer of compressedOut should return the buffer for the compressed
// message to the global pool.
func compressLoop(logger *zap.SugaredLogger, encodedIn <-chan binaryPackage, compressedOut chan binaryPackage) {
	defer func() {
		logger.Debugf("compressLoop exiting")
		close(compressedOut)
	}()

	preCompressionCounter := wiretapSentBytes.With(prometheus.Labels{"compressed": "false"})
	postCompressionCounter := wiretapSentBytes.With(prometheus.Labels{"compressed": "true"})

	buf := make([]byte, 0)
	bbuf := bytes.NewBuffer(buf)
	zw := gzip.NewWriter(bbuf)

	for msg := range encodedIn {
		logger.Debugf("compress: got package %+v, compressing %d bytes: %q", msg, len(msg.payload), msg.payload)

		// Get a buffer, re-initialize the gzip writer on that.
		buf = pool.GlobalPool.Get(1024 * 512)
		bbuf = bytes.NewBuffer(buf)
		bbuf.Reset()
		zw.Reset(bbuf)

		_, err := zw.Write(msg.payload)
		if err != nil {
			panic(fmt.Sprintf("compress: unable to compress: %s", err))
		}
		if err = zw.Close(); err != nil {
			panic(fmt.Sprintf("compress: unable to compress: %s", err))
		}

		logger.Debugf("compress: compressed from %d to %d bytes: %s", len(msg.payload), bbuf.Len(), hex.EncodeToString(bbuf.Bytes()))

		// Record in Prometheus.
		preCompressionCounter.Add(float64(len(msg.payload)))
		postCompressionCounter.Add(float64(bbuf.Len()))

		// Release the message byte buffer from the encoder.
		pool.GlobalPool.Put(msg.payload)

		compressedOut <- binaryPackage{
			payload:     bbuf.Bytes(),
			numEvents:   msg.numEvents,
			contentType: msg.contentType,
		}
	}
}

// Asynchronous loop to take gzipped message payloads and publish them to
// RabbitMQ.
// This exits and closes closedChan when msgOut is closed.
func sendLoop(logger *zap.SugaredLogger, monitorName string, addr string, conn *amqp.Connection, ch *amqp.Channel, msgOut <-chan binaryPackage, closedChan chan struct{}) {
	defer func() {
		logger.Info("sendLoop exiting")
		close(closedChan)
	}()
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	var err error
	routingKeyBitswapMessages := fmt.Sprintf("monitor.%s.bitswap_messages", monitorName)
	routingKeyConnEvents := fmt.Sprintf("monitor.%s.conn_events", monitorName)
	var routingKey string

	nonDroppedEvents := wiretapProcessedEvents.With(prometheus.Labels{"dropped": "false"})
	droppedEvents := wiretapProcessedEvents.With(prometheus.Labels{"dropped": "true"})

	for msg := range msgOut {
		logger.Debugf("send: got message %+v with %d bytes payload", msg, len(msg.payload))

		// Reconnect?
		if conn == nil || conn.IsClosed() || ch == nil || ch.IsClosed() {
			for {
				logger.Debugf("send: need to reconnect...")
				conn, ch, err = setup(addr, logger)
				if err != nil {
					logger.Errorf("unable to connect to RabbitMQ: %s", err)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				break
			}
		}

		// Package and send compressed messages
		pub := amqp.Publishing{
			DeliveryMode: amqp.Transient,
			Body:         msg.payload,
		}

		if msg.contentType == eventTypeBitswapMessages {
			routingKey = routingKeyBitswapMessages
		} else {
			routingKey = routingKeyConnEvents
		}

		// This is not a mandatory delivery, so it will be dropped if there are
		// no queues bound to the exchange (and routing key?).
		err = ch.PublishWithContext(ctx, ExchangeName, routingKey, false, false, pub)

		numBytes := len(msg.payload)

		// Recycle the buffer.
		// TODO from reading the rabbitmq library code, this _should_ be safe to reuse here. We'll have to see about that...
		pool.GlobalPool.Put(msg.payload)

		if err != nil {
			droppedEvents.Add(float64(msg.numEvents))
			logger.Errorf("unable to publish to RabbitMQ, reconnecting: %s", err)
			// Reconnect on next iteration
			conn = nil
			continue
		}

		nonDroppedEvents.Add(float64(msg.numEvents))
		logger.Debugf("send: published message with %d bytes", numBytes)
	}
}
