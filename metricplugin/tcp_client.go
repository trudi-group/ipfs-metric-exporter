package metricplugin

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	pool "github.com/libp2p/go-buffer-pool"
	"github.com/libp2p/go-msgio"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
)

// A client, from the perspective of the server.
type tcpClient struct {
	// The address of the client.
	remote net.Addr

	// The TCP connection.
	conn net.Conn

	// A 4-byte, big-endian frame-delimited writer.
	writer msgio.WriteCloser

	// A 4-byte, big-endian frame-delimited reader.
	reader msgio.ReadCloser

	// Buffered channel for outgoing messages.
	// This dictates backpressure.
	msgOut chan outgoingTCPMessage

	// A lock used to coordinate shutting down.
	closingLock sync.Mutex
	// Closed after a shutdown has been initiated.
	closed chan struct{}
}

func newClient(c net.Conn, closing <-chan struct{}) (*tcpClient, error) {
	client := &tcpClient{
		remote: c.RemoteAddr(),
		conn:   c,
		writer: msgio.NewWriter(c),
		reader: msgio.NewReader(c),
		closed: make(chan struct{}),
		msgOut: make(chan outgoingTCPMessage, 100),
	}

	// Perform a rudimentary handshake.
	err := client.handshake()
	if err != nil {
		log.Errorf("handshake failed with client %s: %s", client.remote, err)
		client.close()
		return nil, err
	}
	log.Debugf("handshake succeeeded with client %s", client.remote)

	// Set up some plumbing
	encodedChan := make(chan []byte)
	compressedChan := make(chan []byte)
	go client.encodeLoop(client.msgOut, encodedChan)
	go client.compressLoop(encodedChan, compressedChan)
	go client.sendLoop(compressedChan)

	go func() {
		<-closing
		client.close()
	}()

	return client, nil
}

// A version message, exchanged between client and server once, immediately
// after the connection is established.
type versionMessage struct {
	Version uint16 `json:"version"`
}

// The current protocol version.
const serverVersion uint16 = 3

// Handshake performs a rudimentary handshake.
// A JSON-encoded, length delimited version message is exchanged in both
// directions.
// If the version does not match, the handshake fails.
func (c *tcpClient) handshake() error {
	buf, err := json.Marshal(versionMessage{Version: serverVersion})
	if err != nil {
		return errors.Wrap(err, "unable to encode version message")
	}

	err = c.sendBytes(buf)
	if err != nil {
		return errors.Wrap(err, "unable to send version message")
	}

	incoming, err := c.reader.ReadMsg()
	if err != nil {
		return errors.Wrap(err, "unable to receive version message")
	}

	incomingMsg := versionMessage{}
	err = json.Unmarshal(incoming, &incomingMsg)
	if err != nil {
		return errors.Wrap(err, "unable to decode version message")
	}
	c.reader.ReleaseMsg(incoming)

	log.Debugf("client %s: got version message %+v", c.remote, incomingMsg)
	if incomingMsg.Version != serverVersion {
		return fmt.Errorf("client version mismatch, expected %d, got %d", serverVersion, incomingMsg.Version)
	}

	return nil
}

// Closes the client connection.
// This is idempotent: A connection will only be closed once, it's safe to call
// this multiple times.
// This will unsubscribe from any events (if set up), close the underlying TCP
// connection, and call the parent server's cleanup function.
func (c *tcpClient) close() {
	// Check if we're already closed.
	c.closingLock.Lock()
	select {
	case <-c.closed:
		return
	default:
	}
	close(c.closed)
	c.closingLock.Unlock()

	log.Infof("closing connection to %s", c.remote)

	// Close the writer, which will also close the TCP connection.
	err := c.writer.Close()
	if err != nil {
		log.With("err", err).Errorf("unable to close TCP connection to client %s?", c.remote)
	}

	log.Debugf("client %s finished close", c.remote)
}

func (c *tcpClient) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// Sends a message via the underlying connection.
// This will JSON-encode, compress, and length-delimit the provided message.
// This does not block
func (c *tcpClient) sendMessage(msg outgoingTCPMessage) error {
	log.Debugf("sending message to client %s: %+v", c.remote, msg)

	// Check for backpressure.
	// We never block, so if we cannot send into the message channel
	// immediately, we return.
	select {
	case c.msgOut <- msg:
		// This is not strictly correct if we can send to msgOut and closed is
		// closed: the channel is buffered, we can potentially send a bunch of
		// messages down the channel even though the client is closed.
		// However, we should hit the c.closed case after at least a few tries.
		return nil
	case <-c.closed:
		return ErrClosed
	default:
		log.Warnf("dropping message to client %s due to backpressure", c.remote)
		return ErrBackpressure
	}
}

// The type sent to via TCP for pushed events.
type event struct {
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

// The type of messages sent out via TCP.
type outgoingTCPMessage struct {
	// If Event is not nil, this message is a pushed event.
	Event *event `json:"event,omitempty"`
}

// Asynchronous loop to encode outgoing messages.
// This takes messages and encodes them to JSON.
// The encoded messages are then pushed into encodedOut.
// For each message, a buffer is taken from the global buffer pool.
// The consumer of encodedOut should return the buffer to the pool.
func (c *tcpClient) encodeLoop(msgIn <-chan outgoingTCPMessage, encodedOut chan []byte) {
	defer func() {
		log.Debugf("encode %s: exiting", c.remote)
		close(encodedOut)
	}()

	var msg outgoingTCPMessage

	for {
		select {
		case msg = <-msgIn:
		case <-c.closed:
			log.Debugf("encode %s: client closed", c.remote)
			return
		}

		log.Debugf("encode %s: encoding message %+v", c.remote, msg)

		buf := pool.GlobalPool.Get(1024 * 512)
		bbuf := bytes.NewBuffer(buf)
		bbuf.Reset()
		w := json.NewEncoder(bbuf)
		if err := w.Encode(msg); err != nil {
			panic(fmt.Sprintf("encode %s: unable to marshal %+v to JSON: %s", c.remote, msg, err))
		}

		log.Debugf("encode %s: encoded outgoing message: %q", c.remote, bbuf.String())

		select {
		case encodedOut <- bbuf.Bytes():
		case <-c.closed:
			log.Debugf("encode %s: client closed", c.remote)
			return
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
func (c *tcpClient) compressLoop(encodedIn <-chan []byte, compressedOut chan []byte) {
	defer func() {
		log.Debugf("compress %s: exiting", c.remote)
		close(compressedOut)
	}()

	preCompressionCounter := wiretapSentBytes.With(prometheus.Labels{"compressed": "false"})
	postCompressionCounter := wiretapSentBytes.With(prometheus.Labels{"compressed": "true"})

	var msgBytes []byte
	buf := make([]byte, 0)
	bbuf := bytes.NewBuffer(buf)
	zw := gzip.NewWriter(bbuf)

	for {
		select {
		case msgBytes = <-encodedIn:
		case <-c.closed:
			log.Debugf("compress %s: client closed", c.remote)
			return
		}

		log.Debugf("compress %s: compressing %d bytes: %q", c.remote, len(msgBytes), msgBytes)

		// Get a buffer, re-initialize the gzip writer on that.
		buf = pool.GlobalPool.Get(1024 * 512)
		bbuf = bytes.NewBuffer(buf)
		bbuf.Reset()
		zw.Reset(bbuf)

		_, err := zw.Write(msgBytes)
		if err != nil {
			panic(fmt.Sprintf("compress %s: unable to compress: %s", c.remote, err))
		}
		if err = zw.Close(); err != nil {
			panic(fmt.Sprintf("compress %s: unable to compress: %s", c.remote, err))
		}

		log.Debugf("compress %s: compressed from %d to %d bytes: %s", c.remote, len(msgBytes), bbuf.Len(), hex.EncodeToString(bbuf.Bytes()))

		// Record in Prometheus.
		preCompressionCounter.Add(float64(len(msgBytes)))
		postCompressionCounter.Add(float64(bbuf.Len()))

		// Release the message byte buffer from the encoder.
		pool.GlobalPool.Put(msgBytes)

		select {
		case compressedOut <- bbuf.Bytes():
		case <-c.closed:
			log.Debugf("compress %s: client closed", c.remote)
			return
		}
	}
}

var (
	// ErrTimeout is returned if a send operation times out.
	ErrTimeout = errors.New("timeout sending")
	// ErrClosed is returned for method calls on a closed or closing client.
	ErrClosed = errors.New("client closed")
	// ErrBackpressure is returned if the outgoing buffer of messages is full.
	ErrBackpressure = errors.New("dropped due to backpressure")
)

// Asynchronous loop to send framed messages to the client.
// This takes a message (as a byte slice) and sends it to the client using
// the length-delimited writer.
func (c *tcpClient) sendLoop(msgIn <-chan []byte) {
	defer func() {
		log.Debugf("send %s: exiting", c.remote)
		c.close()
	}()

	var msgBytes []byte
	for {
		select {
		case msgBytes = <-msgIn:
		case <-c.closed:
			log.Debugf("send %s: client closed", c.remote)
			return
		}

		log.Debugf("send %s: sending %d bytes", c.remote, len(msgBytes))

		err := c.sendBytes(msgBytes)
		if err != nil {
			if err != ErrClosed {
				log.Errorf("send %s: unable to send: %s", c.remote, err)
			}
			return
		}

		// Recycle the buffer
		pool.GlobalPool.Put(msgBytes)
	}
}

func (c *tcpClient) sendBytes(b []byte) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}

	log.Debugf("attempting to send %d bytes to %s: %v", len(b), c.remote, hex.EncodeToString(b))

	// Send  asynchronously with a length prefix.
	sendResultChan := make(chan error)
	go func() {
		defer func() { close(sendResultChan) }()

		select {
		case <-c.closed:
			sendResultChan <- ErrClosed
			return
		default:
		}

		err := c.writer.WriteMsg(b)

		select {
		case sendResultChan <- err:
		case <-c.closed:
		}
	}()

	// We set up a crude write timeout.
	timer := time.NewTimer(10 * time.Second)
	select {
	case err := <-sendResultChan:
		// Send finished, cancel timer.
		if !timer.Stop() {
			<-timer.C
		}

		if err != nil {
			return err
		}
	case <-timer.C:
		// Timeout
		log.Errorf("timeout writing to %s", c.remote)

		return ErrTimeout
	case <-c.closed:
		// Client closed
		return ErrClosed
	}

	log.Debugf("sent message to client %s", c.remote)
	return nil
}
