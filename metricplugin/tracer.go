package metricplugin

import (
	"sync"
	"time"

	bsmsg "github.com/ipfs/boxo/bitswap/message"
	pbmsg "github.com/ipfs/boxo/bitswap/message/pb"
	"github.com/ipfs/boxo/bitswap/tracer"
	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/pkg/errors"
)

var (
	_ tracer.Tracer    = (*BitswapTracer)(nil)
	_ network.Notifiee = (*BitswapTracer)(nil)
)

// BitswapTracer implements the `bitswap.Tracer` interface to log Bitswap
// messages.
type BitswapTracer struct {
	nodeNetwork network.Network

	// RabbitMQ client to publish messages.
	rmq *RabbitMQPublisher

	// Lifetime management.
	closing   chan struct{}
	closeLock sync.Mutex
	wg        sync.WaitGroup
}

// NewTracer creates a new Bitswap Tracer.
// Use the Shutdown method to shut down cleanly.
func NewTracer(nodeNetwork network.Network, monitorName string, amqpServerAddress string) (*BitswapTracer, error) {
	rmq, err := newRabbitMQPublisher(monitorName, amqpServerAddress)
	if err != nil {
		return nil, errors.Wrap(err, "unable to connect to AMQP server")
	}

	wt := &BitswapTracer{
		nodeNetwork: nodeNetwork,
		rmq:         rmq,
		closing:     make(chan struct{}),
	}

	return wt, nil
}

// Shutdown shuts down the tracer cleanly.
// This is idempotent, i.e. calling it multiple times does not cause problems.
func (wt *BitswapTracer) Shutdown() {
	wt.closeLock.Lock()
	select {
	case <-wt.closing:
	// Already closing
	default:
		close(wt.closing)
	}
	wt.closeLock.Unlock()

	wt.wg.Wait()
}

// MessageReceived is called on incoming Bitswap messages.
func (wt *BitswapTracer) MessageReceived(peerID peer.ID, msg bsmsg.BitSwapMessage) {
	now := time.Now()

	// Get potential underlay addresses from current connections to the peer.
	// We explicitly allocate an empty slice because the ConnectedAddresses
	// field on the messages we log (and send out via TCP) is non-nullable.
	// It apparently sometimes happens that we don't find an open connection
	// to the peer we received from (race condition, yay).
	potentialAddresses := make([]ma.Multiaddr, 0, 1)
	// TODO we have to keep an eye on performance: This read-locks the
	// connection manager(? or something else) and allocates a slice.
	// Copying a few addresses is probably fast, but this might become a problem
	// if we receive multiple thousand messages per second.
	conns := wt.nodeNetwork.ConnsToPeer(peerID)
	for _, c := range conns {
		potentialAddresses = append(potentialAddresses, c.RemoteMultiaddr())
	}

	// Get CIDs of contained blocks.
	msgBlocks := msg.Blocks()
	blocks := make([]cid.Cid, 0, len(msgBlocks))
	for _, block := range msgBlocks {
		blocks = append(blocks, block.Cid())
	}

	// Get block presences.
	msgBlockPresences := msg.BlockPresences()
	blockPresences := make([]BlockPresence, 0, len(msgBlockPresences))
	for _, presence := range msgBlockPresences {
		p := BlockPresence{
			Cid: presence.Cid,
		}
		if presence.Type == pbmsg.Message_Have {
			p.Type = Have
		} else {
			p.Type = DontHave
		}
		blockPresences = append(blockPresences, p)
	}

	// Construct the message to push to subscribers
	msgToLog := BitswapMessage{
		WantlistEntries:    msg.Wantlist(),
		FullWantList:       msg.Full(),
		Blocks:             blocks,
		BlockPresences:     blockPresences,
		ConnectedAddresses: potentialAddresses,
	}
	log.Debugf("Received Bitswap message from peer %s: %+v", peerID, msgToLog)

	// Push out via RabbitMQ.
	select {
	case <-wt.closing:
		// We're closing or closed, don't attempt to send via RabbitMQ.
		return
	default:
	}
	wt.rmq.PublishBitswapMessage(now, peerID, &msgToLog)
}

// MessageSent is called on outgoing Bitswap messages.
// We do not use this at the moment.
func (*BitswapTracer) MessageSent(peer.ID, bsmsg.BitSwapMessage) {}

// Listen implements network.Notifiee.
// We do not use this at the moment.
func (*BitswapTracer) Listen(network.Network, ma.Multiaddr) {}

// ListenClose implements network.Notifiee.
// We do not use this at the moment.
func (*BitswapTracer) ListenClose(network.Network, ma.Multiaddr) {}

// Connected implements network.Notifiee.
func (wt *BitswapTracer) Connected(_ network.Network, conn network.Conn) {
	wt.rmq.PublishConnectionEvent(time.Now(), true, conn)
}

// Disconnected implements network.Notifiee.
func (wt *BitswapTracer) Disconnected(_ network.Network, conn network.Conn) {
	wt.rmq.PublishConnectionEvent(time.Now(), false, conn)
}
