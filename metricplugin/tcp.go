package metricplugin

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-msgio"
	"github.com/pkg/errors"
)

type tcpServer struct {
	// The API of the parent plugin.
	plugin PluginAPI

	// A channel shared with all clients to notify them if we're shutting down.
	closing chan struct{}
	// A wait group to keep track of the clients.
	wg sync.WaitGroup

	// The underlying listener.
	listener *net.TCPListener
}

// TCPServerConfig is the config for the metric export TCP server.
type TCPServerConfig struct {
	// ListenAddress specifies the address to listen on.
	// It should take a form usable by `net.ResolveTCPAddr`, for example
	// `localhost:8181` or `0.0.0.0:1444`.
	ListenAddress string `json:"ListenAddress"`
}

func newTCPServer(plugin PluginAPI, cfg TCPServerConfig) (*tcpServer, error) {
	addr, err := net.ResolveTCPAddr("tcp", cfg.ListenAddress)
	if err != nil {
		return nil, errors.Wrap(err, "unable to resolve listenAddress as TCP address")
	}
	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return nil, errors.Wrap(err, "unable to bind")
	}
	log.Infof("metric export TCP API running on %s", listener.Addr())

	return &tcpServer{
		plugin:   plugin,
		listener: listener,
		closing:  make(chan struct{}),
		wg:       sync.WaitGroup{},
	}, nil
}

func (s *tcpServer) listenAndServe() {
	// See `Shutdown` about how this all works together.
	defer func() {
		log.Infof("cleaning up TCP clients")

		close(s.closing)
	}()

	for {
		// Accept a new connection.
		c, err := s.listener.AcceptTCP()
		if err != nil {
			// We search for this error string to figure out if we're shutting
			// down :)
			if !strings.Contains(err.Error(), "use of closed network connection") {
				log.Error("unable to accept connection: ", err)
			}
			return
		}
		log.Infof("new connection from %s", c.RemoteAddr())

		remote := c.RemoteAddr()
		client := tcpClient{
			plugin:          s.plugin,
			conn:            c,
			remote:          remote,
			writer:          msgio.NewWriter(c),
			closed:          make(chan struct{}),
			serverClosing:   s.closing,
			serverWaitGroup: &s.wg,
		}

		// Keep track of the connection and let it run.
		s.wg.Add(1)
		go client.run()
	}
}

// Shutdown shuts down the TCP server and all connected clients.
// This will close the listener as well as all currently open connections.
// This blocks until all connections are closed, but that shouldn't take long.
func (s *tcpServer) Shutdown() {
	log.Info("shutting down TCP server")

	// Shut down the listener.
	// This should cause `AcceptTCP` to error out, which then triggers the
	// deferred function in `listenAndServe`.
	// That, in turn, closes the closing channel, which will cause all clients
	// to shut down.
	// Each client should then clean up one entry in the `WaitGroup`, which
	// brings us back here, where we'll wait for all of them to be done.
	err := s.listener.Close()
	if err != nil {
		log.Errorw("unable to close TCP listener?", "err", err)
	}

	s.wg.Wait()
	log.Info("TCP server shut down")
}

var _ EventSubscriber = (*tcpClient)(nil)

// A client.
type tcpClient struct {
	// The parent plugin, to execute commands.
	plugin PluginAPI

	// The address of the client.
	remote net.Addr

	// The TCP connection.
	conn *net.TCPConn

	// A 4-byte, big-endian frame-delimited writer.
	writer msgio.Writer

	// Channel to keep track of whether the server is shutting down.
	// When this is closed, the connection will close itself and
	// decrement the parent's wait group.
	serverClosing   chan struct{}
	serverWaitGroup *sync.WaitGroup

	// A lock used to coordinate shutting down.
	lock sync.Mutex
	// Closed after a shutdown has been initiated.
	closed chan struct{}
}

// A version message, exchanged between client and server once, immediately
// after the connection is established.
type versionMessage struct {
	Version uint16 `json:"version"`
}

// The version of this server.
const serverVersion uint16 = 2

// Handshake performs a rudimentary handshake.
// A JSON-encoded, length delimited version message is exchanged in both
// directions.
// If the version does not match, the handshake fails.
func (c *tcpClient) handshake(reader msgio.Reader) error {
	buf, err := json.Marshal(versionMessage{Version: serverVersion})
	if err != nil {
		return errors.Wrap(err, "unable to encode version message")
	}

	err = c.writer.WriteMsg(buf)
	if err != nil {
		return errors.Wrap(err, "unable to send version message")
	}

	incoming, err := reader.ReadMsg()
	if err != nil {
		return errors.Wrap(err, "unable to receive version message")
	}

	incomingMsg := versionMessage{}
	err = json.Unmarshal(incoming, &incomingMsg)
	if err != nil {
		return errors.Wrap(err, "unable to decode version message")
	}
	reader.ReleaseMsg(incoming)

	log.Debugf("client %s: got version message %+v", c.remote, incomingMsg)
	if incomingMsg.Version != serverVersion {
		return fmt.Errorf("client version mismatch, expected %d, got %d", serverVersion, incomingMsg.Version)
	}

	return nil
}

// Runs the client, blocking.
// This does some initial setup, performs a handshake, subscribes to events,
// and then loops on incoming messages to handle them as requests.
func (c *tcpClient) run() {
	go func() {
		<-c.serverClosing
		c.close()
	}()

	// Perform a rudimentary handshake.
	reader := msgio.NewReader(c.conn)
	err := c.handshake(reader)
	if err != nil {
		log.Errorf("handshake failed with client %s: %s", c.remote, err)
		c.close()
		return
	}
	log.Debugf("handshake succeeeded with client %s", c.remote)

	// Read and process incoming messages.
	for {
		// Read message.
		msgBytes, err := reader.ReadMsg()
		if err != nil {
			// Are we shutting down?
			select {
			case <-c.closed:
				log.Debugf("reader %s: shutting down", c.remote)
				return
			default:
			}

			if err == io.EOF {
				// This is usually a graceful shutdown, i.e. the remote properly
				// closed the TCP connection.
				log.Debugf("got EOF reading from client %s, closing", c.remote)
			} else {
				// This is usually unplanned.
				log.Errorf("unable to read from client %s: %s", c.remote, err)
			}

			// Close the connection.
			c.close()
			return
		}
		log.Debugf("read data from %s: %s", c.remote, string(msgBytes))

		// Decode the message.
		msg := incomingTCPMessage{}
		err = json.Unmarshal(msgBytes, &msg)
		if err != nil {
			log.Warnf("unable to decode request from client %s: %s", c.remote, err)

			// Drop clients that send garbage.
			c.close()
			return
		}
		log.Debugf("decoded message from %s: %+v", c.remote, msg)

		// Recycle the buffer for performance.
		reader.ReleaseMsg(msgBytes)

		// Handle the request.
		if msg.Request != nil {
			outMsg := outgoingTCPMessage{Response: &response{ID: msg.Request.ID}}

			if msg.Request.Ping != nil {
				c.plugin.Ping()
				outMsg.Response.Ping = &pingResponse{}
			} else if msg.Request.Subscribe != nil {
				err = c.plugin.Subscribe(c)
				errMsg := ""
				if err != nil {
					errMsg = err.Error()
				}
				outMsg.Response.Subscribe = &subscribeResponse{Error: &errMsg}
			} else if msg.Request.Unsubscribe != nil {
				c.plugin.Unsubscribe(c)
				outMsg.Response.Unsubscribe = &unsubscribeResponse{}
			} else {
				log.Warnf("client %s sent empty request", c.remote)

				// Drop clients that send garbage.
				c.close()
				return
			}

			c.sendMessage(outMsg)
		}
	}
}

// Closes the client connection.
// This is idempotent: A connection will only be closed once, it's safe to call
// this multiple times.
// This will unsubscribe from any events (if set up), close the underlying TCP
// connection, and call the parent server's cleanup function.
func (c *tcpClient) close() {
	// Check if we're already closed.
	c.lock.Lock()
	select {
	case <-c.closed:
		return
	default:
	}
	close(c.closed)
	c.lock.Unlock()

	log.Infof("closing connection to %s", c.remote)

	// Unsubscribe, to clean up.
	c.plugin.Unsubscribe(c)

	// Close the TCP connection.
	err := c.conn.Close()
	if err != nil {
		log.With("err", err).Errorf("unable to close TCP connection to client %s?", c.remote)
	}

	// Notify parent server that we're gone.
	c.serverWaitGroup.Done()
	log.Debugf("client %s finished close", c.remote)
}

// ID generates a unique ID for this client.
func (c *tcpClient) ID() string {
	return fmt.Sprintf("tcp-%s", c.remote)
}

// The type of messages coming in via TCP.
type incomingTCPMessage struct {
	// If Request is not nil, this message is a request.
	Request *request `json:"request,omitempty"`
}

type request struct {
	// The ID of the request.
	// This must be unique per client per point in time.
	// Request IDs are used to match responses to requests.
	// They may be reused after a response to an earlier request has been
	// received.
	ID uint16 `json:"id"`

	// If Subscribe is not nil, this is a Subscribe request.
	// See the PluginAPI interface.
	Subscribe *subscribeRequest `json:"subscribe,omitempty"`

	// If Unsubscribe is not nil, this is an Unsubscribe request.
	// See the PluginAPI interface.
	Unsubscribe *unsubscribeRequest `json:"unsubscribe,omitempty"`

	// If Ping is not nil, this is a Ping request.
	// See the PluginAPI interface.
	Ping *pingRequest `json:"ping,omitempty"`
}

type subscribeRequest struct{}

type unsubscribeRequest struct{}

type pingRequest struct{}

// The type of messages sent out via TCP.
type outgoingTCPMessage struct {
	// If Event is not nil, this message is a pushed event.
	Event *event `json:"event,omitempty"`

	// If Response is not nil, this message is a response to an earlier request.
	Response *response `json:"response,omitempty"`
}

type response struct {
	// The ID of the response.
	// This matches the response to an earlier request.
	ID uint16 `json:"id"`

	// If Subscribe is not nil, this message is the response to a
	// Subscribe request.
	Subscribe *subscribeResponse `json:"subscribe,omitempty"`

	// If Unsubscribe is not nil, this message is the response to a
	// Unsubscribe request.
	Unsubscribe *unsubscribeResponse `json:"unsubscribe,omitempty"`

	// If Ping is not nil, this message is the response to a
	// Ping request.
	Ping *pingResponse `json:"ping,omitempty"`
}

type subscribeResponse struct {
	// Propagates the error in string representation from the Subscribe method.
	Error *string `json:"error,omitempty"`
}

type unsubscribeResponse struct{}

type pingResponse struct{}

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

// BitswapMessageReceived implements the EventSubscriber interface.
func (c *tcpClient) BitswapMessageReceived(ts time.Time, peer peer.ID, msg BitswapMessage) {
	tcpMsg := outgoingTCPMessage{
		Event: &event{
			Timestamp:       ts,
			Peer:            peer.Pretty(),
			BitswapMessage:  &msg,
			ConnectionEvent: nil,
		},
	}

	c.sendMessage(tcpMsg)
}

// ConnectionEventRecorded implements the EventSubscriber interface.
func (c *tcpClient) ConnectionEventRecorded(ts time.Time, peer peer.ID, connEvent ConnectionEvent) {
	tcpMsg := outgoingTCPMessage{
		Event: &event{
			Timestamp:       ts,
			Peer:            peer.Pretty(),
			BitswapMessage:  nil,
			ConnectionEvent: &connEvent,
		},
	}

	c.sendMessage(tcpMsg)
}

// Sends a message via the underlying connection.
// This will JSON-encode and length-delimit the provided message.
// This blocks (for now). TODO maybe change this.
func (c *tcpClient) sendMessage(msg outgoingTCPMessage) {
	log.Debugf("sending message to client %s: %+v", c.remote, msg)

	// Encode as JSON.
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		panic(fmt.Sprintf("unable to marshal JSON: %s", err))
	}
	log.Debugf("encoded outgoing message to client %s: %s", c.remote, string(msgBytes))

	// Send with a length prefix.
	err = c.writer.WriteMsg(msgBytes)
	if err != nil {
		log.Errorf("unable to write to %s: %s", c.remote, err)
		// Kill it, I guess?
		c.close()
	}
	log.Debugf("sent message to client %s", c.remote)

	// TODO reuse the byte slice
}
