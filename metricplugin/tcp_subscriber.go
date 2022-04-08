package metricplugin

import (
	"fmt"
	"net"
	"time"

	"github.com/libp2p/go-libp2p-core/peer"
)

var _ EventSubscriber = (*tcpSubscriber)(nil)

// A subscriber to the Bitswap monitoring, connected via TCP.
type tcpSubscriber struct {
	c       *tcpClient
	closing <-chan struct{}
}

func newTCPSubscriber(c net.Conn, closing <-chan struct{}) (*tcpSubscriber, error) {
	client, err := newClient(c, closing)
	if err != nil {
		return nil, err
	}

	handler := &tcpSubscriber{
		c:       client,
		closing: closing,
	}

	return handler, nil
}

// Run blocks for as long as the client is running.
func (c *tcpSubscriber) run() {
	<-c.closing
}

// ID generates a unique ID for this client.
func (c *tcpSubscriber) ID() string {
	return fmt.Sprintf("tcp-%s", c.c.remote)
}

// BitswapMessageReceived implements the EventSubscriber interface.
func (c *tcpSubscriber) BitswapMessageReceived(ts time.Time, peer peer.ID, msg BitswapMessage) error {
	if c.c.isClosed() {
		return ErrClosed
	}

	tcpMsg := outgoingTCPMessage{
		Event: &event{
			Timestamp:       ts,
			Peer:            peer.Pretty(),
			BitswapMessage:  &msg,
			ConnectionEvent: nil,
		},
	}

	if err := c.c.sendMessage(tcpMsg); err != nil {
		if err != ErrBackpressure {
			log.Warnf("unable to send Bitswap message to %s: %s", c.c.remote, err)
			// Kill it, I guess?
			c.c.close()
			return err
		}
	}

	return nil
}

// ConnectionEventRecorded implements the EventSubscriber interface.
func (c *tcpSubscriber) ConnectionEventRecorded(ts time.Time, peer peer.ID, connEvent ConnectionEvent) error {
	if c.c.isClosed() {
		return ErrClosed
	}

	tcpMsg := outgoingTCPMessage{
		Event: &event{
			Timestamp:       ts,
			Peer:            peer.Pretty(),
			BitswapMessage:  nil,
			ConnectionEvent: &connEvent,
		},
	}

	if err := c.c.sendMessage(tcpMsg); err != nil {
		if err != ErrBackpressure {
			log.Warnf("unable to send connection event to %s: %s", c.c.remote, err)
			// Kill it, I guess?
			c.c.close()
			return err
		}
	}

	return nil
}
