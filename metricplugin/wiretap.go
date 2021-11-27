package metricplugin

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/ipfs/go-bitswap"
	bsmsg "github.com/ipfs/go-bitswap/message"
	pbmsg "github.com/ipfs/go-bitswap/message/pb"
	bsnet "github.com/ipfs/go-bitswap/network"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-ipfs/core"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	kadDHTString = "/kad/"
)

var (
	_ bitswap.WireTap  = (*BitswapWireTap)(nil)
	_ network.Notifiee = (*BitswapWireTap)(nil)
)

// BitswapWireTap implements the `bitswap.WireTap` interface to log Bitswap
// messages.
// It also implements `network.Notifiee` to receive notifications about peers
// connecting, disconnecting, etc.
// It keeps track of peers the node is connected to and attempts to hold an
// active Bitswap sender to each of them.
//
// Using the `network.Notifiee` events is somewhat wonky: We occasionally
// receive duplicate disconnection events or miss connection events.
// However, it looks like we converge with the true IPFS connectivity after a
// while.
type BitswapWireTap struct {
	api       *core.IpfsNode
	bsnetImpl bsnet.BitSwapNetwork

	// Keeps track of whom to send Bitswap messages and connection events to.
	subscribers subscribers

	// Keeps track of peers we are connected to and bitswap senders.
	connManager connectionManager

	// Lifetime management.
	closing   chan struct{}
	closeLock sync.Mutex
	wg        sync.WaitGroup
}

// Tracks which peers we are connected to, and whether we have a Bitswap sender
// to them.
type connectionManager struct {
	lock           sync.Mutex
	allConnections map[string]struct{}
	peers          map[peer.ID]peerConnection
}

// Tracks connections to a single peer and whether we have a Bitswap sender to
// this peer.
type peerConnection struct {
	bitswapSender bsnet.MessageSender
	connections   map[string]network.Conn
}

type subscribers struct {
	subscribers []EventSubscriber
	lock        sync.RWMutex
}

// NewWiretap creates a new Bitswap Wiretap and network Notifiee.
// This will spawn goroutines to log stats and connect to peers via Bitswap.
// Use the Shutdown method to shut down cleanly.
func NewWiretap(ipfsInstance *core.IpfsNode, bsnetImpl bsnet.BitSwapNetwork) *BitswapWireTap {
	wt := &BitswapWireTap{
		api:       ipfsInstance,
		bsnetImpl: bsnetImpl,
		connManager: connectionManager{
			allConnections: make(map[string]struct{}),
			peers:          make(map[peer.ID]peerConnection),
		},
	}

	// Start a goroutine to periodically go through all peers and open Bitswap
	// senders if we're missing any.
	wt.wg.Add(1)
	go wt.bitswapConnectionLoop()

	// Start a goroutine to periodically print stats about our perceived
	// connectivity and Bitswap senders.
	// This also populates prometheus.
	wt.wg.Add(1)
	go wt.connectivityStatLoop()

	return wt
}

// WaitGroup-bound function to loop forever and print stats about connectivity
// and Bitswap senders.
// This exits when wt.closing is closed.
func (wt *BitswapWireTap) connectivityStatLoop() {
	defer wt.wg.Done()

	// A list of Bitswap protocol IDs.
	// These are the official IPFS implementations of Bitswap.
	// There are more (e.g., ipfs-embed), but we can't speak those.
	bitswapProtocols := map[protocol.ID]struct{}{
		bsnet.ProtocolBitswapNoVers:  {},
		bsnet.ProtocolBitswapOneZero: {},
		bsnet.ProtocolBitswapOneOne:  {},
		bsnet.ProtocolBitswap:        {},
	}

	// Ticker for periodic printing/prometheus populating.
	ticker := time.NewTicker(time.Duration(10) * time.Second)

	for {
		select {
		case <-wt.closing:
			log.Debug("Wiretap connectivity stat loop exiting.")
			return
		case <-ticker.C:
		}

		// Get stats as reported by the IPFS node.
		conns := wt.api.PeerHost.Network().Conns()
		peers := make(map[peer.ID]struct{})
		numBitswapStreams := 0
		for _, conn := range conns {
			peers[conn.RemotePeer()] = struct{}{}

			streams := conn.GetStreams()
			for _, stream := range streams {
				proto := stream.Protocol()
				if _, ok := bitswapProtocols[proto]; ok {
					numBitswapStreams++
				}
			}
		}

		// Get stats as seen by us.
		wt.connManager.lock.Lock()
		numOurPeers := len(wt.connManager.peers)
		numOurBitswapSenders := 0
		for _, peerConn := range wt.connManager.peers {
			if peerConn.bitswapSender != nil {
				numOurBitswapSenders++
			}
		}
		numOurConns := len(wt.connManager.allConnections)
		wt.connManager.lock.Unlock()

		// Log some useful info for debugging.
		log.Infof("IPFS reports %d connections to %d peers, with %d bitswap streams. We track %d connections to %d peers (%d bitswap senders)",
			len(conns), len(peers), numBitswapStreams, numOurConns, numOurPeers, numOurBitswapSenders)

		// Populate prometheus.
		wiretapBitswapSenderCount.Set(float64(numOurBitswapSenders))
		wiretapPeerCount.Set(float64(numOurPeers))
		wiretapConnectionCount.Set(float64(numOurConns))
	}
}

// WaitGroup-bound function to periodically create missing Bitswap senders.
// This will go through all peers we believe we are connected to and creates a
// goroutine for each missing Bitswap sender, calling connectBitswap.
// Exits when wt.closing is closed.
func (wt *BitswapWireTap) bitswapConnectionLoop() {
	defer wt.wg.Done()

	// Ticker for periodic Bitswap sender creation.
	ticker := time.NewTicker(time.Duration(30) * time.Second)

	for {
		select {
		case <-wt.closing:
			log.Debug("bitswap connection loop exiting")
			return
		case <-ticker.C:
		}

		wt.connManager.lock.Lock()
		for peerID, peerConn := range wt.connManager.peers {
			if peerConn.bitswapSender == nil {
				// We use some random sleep duration before connecting.
				// I hope that makes things nicer.
				wt.wg.Add(1)
				go wt.connectBitswap(peerID, time.Duration(rand.Intn(5000))*time.Millisecond)
			}
		}
		wt.connManager.lock.Unlock()
	}
}

func (wt *BitswapWireTap) subscribe(subscriber EventSubscriber) error {
	wt.subscribers.lock.Lock()
	defer wt.subscribers.lock.Unlock()

	// TODO we could make this sorted and then binary search, but not worth now.
	id := subscriber.ID()
	for _, sub := range wt.subscribers.subscribers {
		if sub.ID() == id {
			return ErrAlreadySubscribed
		}
	}

	wt.subscribers.subscribers = append(wt.subscribers.subscribers, subscriber)
	return nil
}

func (wt *BitswapWireTap) unsubscribe(subscriber EventSubscriber) {
	wt.subscribers.lock.Lock()
	defer wt.subscribers.lock.Unlock()

	// TODO we could make this sorted and then binary search, but not worth now.
	id := subscriber.ID()
	for i, sub := range wt.subscribers.subscribers {
		if sub.ID() == id {
			wt.subscribers.subscribers = append(wt.subscribers.subscribers[:i], wt.subscribers.subscribers[i+1:]...)
			return
		}
	}
}

// Shutdown shuts down the wiretap cleanly.
// This is idempotent, i.e. calling it multiple times does not cause problems.
// This will block until all goroutines have quit (this depends on some
// timeouts for connecting to peers and opening streams, probably).
func (wt *BitswapWireTap) Shutdown() {
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
func (wt *BitswapWireTap) MessageReceived(pid peer.ID, msg bsmsg.BitSwapMessage) {
	/* 	conns := wt.api.PeerHost.Network().ConnsToPeer(pid)
	   	// Unpack the multiaddresses
	   	var mas []ma.Multiaddr
	   	for _, c := range conns {
	   		mas = append(mas, c.RemoteMultiaddr())
	   	}
	   	fmt.Printf("Received msg from %s with multiaddrs: %s \n", pid, mas) */

	now := time.Now()

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
		WantlistEntries: msg.Wantlist(),
		FullWantList:    msg.Full(),
		Blocks:          blocks,
		BlockPresences:  blockPresences,
	}

	// This _will_ block if one of the subscribers blocks.
	// We'll have to see if that's a problem.
	wt.subscribers.lock.RLock()
	defer wt.subscribers.lock.RUnlock()
	for _, sub := range wt.subscribers.subscribers {
		sub.BitswapMessageReceived(now, pid, msgToLog)
	}
}

// MessageSent is called on outgoing Bitswap messages.
// We do not use this at the moment.
func (*BitswapWireTap) MessageSent(peer.ID, bsmsg.BitSwapMessage) {}

// Listen is called when the network implementation starts listening on the given address.
// We do not use this at the moment.
func (*BitswapWireTap) Listen(network.Network, ma.Multiaddr) {}

// ListenClose is called when the network implementation stops listening on the given address.
// We do not use this at the moment.
func (*BitswapWireTap) ListenClose(network.Network, ma.Multiaddr) {}

func (wt *BitswapWireTap) notifyConnEvent(connected bool, conn network.Conn) {
	now := time.Now()
	eventToLog := ConnectionEvent{
		Remote:              conn.RemoteMultiaddr(),
		ConnectionEventType: Connected,
	}
	if !connected {
		eventToLog.ConnectionEventType = Disconnected
	}
	p := conn.RemotePeer()

	// This _will_ block if one of the subscribers blocks.
	// We'll have to see if that's a problem.
	wt.subscribers.lock.RLock()
	defer wt.subscribers.lock.RUnlock()
	for _, sub := range wt.subscribers.subscribers {
		sub.ConnectionEventRecorded(now, p, eventToLog)
	}
}

// Connected is called when a connection is opened.
func (wt *BitswapWireTap) Connected(_ network.Network, conn network.Conn) {
	remotePeer := conn.RemotePeer()
	log.Debugf("Connection event for peer %s, address: %s", remotePeer, conn.RemoteMultiaddr())

	// Track connection/peer connectivity.
	wt.connManager.lock.Lock()
	defer wt.connManager.lock.Unlock()
	if _, ok := wt.connManager.allConnections[conn.ID()]; ok {
		log.Warnf("Duplicate connection ID %s (Duplicate Connection Event)", conn.ID())
		// What now?
	}
	wt.connManager.allConnections[conn.ID()] = struct{}{}

	peerConn, ok := wt.connManager.peers[remotePeer]
	if !ok {
		peerConn = peerConnection{
			bitswapSender: nil,
			connections:   map[string]network.Conn{conn.ID(): conn},
		}
	} else {
		if _, ok := peerConn.connections[conn.ID()]; ok {
			log.Warnf("Duplicate connection ID %s to peer %s", conn.ID(), remotePeer)
			// What now?
		}
		peerConn.connections[conn.ID()] = conn
	}
	wt.connManager.peers[remotePeer] = peerConn

	// Connect via Bitswap
	// Check if we're shutting down
	select {
	case <-wt.closing:
		log.Debugf("Connection event handler: not starting bitswap connector to %s because we're shutting down", conn.ID())
	default:
		// Open Bitswap connection in the background
		wt.wg.Add(1)
		go wt.connectBitswap(conn.RemotePeer(), time.Duration(1)*time.Second)
	}

	wt.notifyConnEvent(true, conn)
}

// Creates a Bitswap sender to a given peer after waiting the specified time.
// This is supposed to be run as a goroutine and will decrement wt.wg!
//
// Creating a Bitswap sender instructs the IPFS node to connect to the given
// peer, open a Bitswap stream, and creates a message sender on top of that
// stream.
// We will check if we're still connected to the peer, to avoid re-opening
// connections.
// We also check if any of the connections we have to a peer are non-transient.
// If we're still connected, we instruct Bitswap to create a new sender.
// This is racy: The connection could have been closed in the meantime.
// In that case, Bitswap will reconnect, which can take some time (or fail).
// We re-check our connectivity to the peer after we created the sender.
// If we're not connected anymore (for whatever reason), we destroy the sender.
//
// It can happen that the connection that triggered the connection event which
// then triggered us to connect via Bitswap is not the connection through which
// the Bitswap sender communicates.
// This is generally not a problem: The Bitswap sender, when instructed to send
// a message, will retry and reconnect to its peer, using whatever connection is
// available to that peer.
func (wt *BitswapWireTap) connectBitswap(remotePeer peer.ID, waitTime time.Duration) {
	defer wt.wg.Done()

	// Wait a few seconds, maybe this was a short-lived connection.
	time.Sleep(waitTime)

	// Check if
	// 1. We're still connected to the peer, and
	// 2. we already have a bitswap sender for that peer
	// 3. we have any non-transient connections to that peer
	wt.connManager.lock.Lock()
	peerEntry, ok := wt.connManager.peers[remotePeer]
	if !ok {
		log.Debugf("bitswap connector: no longer connected to peer %s, not opening a sender", remotePeer)
		wt.connManager.lock.Unlock()
		return
	}
	if peerEntry.bitswapSender != nil {
		log.Debugf("bitswap connector: already have a sender to peer %s, not opening a sender", remotePeer)
		wt.connManager.lock.Unlock()
		return
	}
	anyNonTransient := false
	for _, conn := range peerEntry.connections {
		if !conn.Stat().Transient {
			anyNonTransient = true
		}
	}
	if !anyNonTransient {
		log.Debugf("bitswap connector: we have only transient connections to peer %s, not opening a sender", remotePeer)
		wt.connManager.lock.Unlock()
		return
	}
	wt.connManager.lock.Unlock()

	// The connection is still alive, and we don't have a sender yet.
	// Try to create a sender and add it to the peer.
	// We don't hold the connManager lock the entire time because connecting
	// can take some time, e.g. if the connection is actually dead.

	// Leave this blank to use defaults
	// TODO we could make this configurable.
	opts := bsnet.MessageSenderOpts{}
	ctx := context.Background()

	log.Debugf("bitswap connector: trying to create sender to peer %s", remotePeer)
	sender, err := wt.bsnetImpl.NewMessageSender(ctx, remotePeer, &opts)
	if err != nil {
		log.Debugf("bitswap connector: unable to create sender to peer %s: %s", remotePeer, err)
		// TODO maybe retry? But the sender retries internally... Also, the
		// periodic bitswap connection goroutine should find this and clean up.
		return
	}
	log.Debugf("bitswap connector: created sender to peer %s", remotePeer)

	// Add the sender to the peer.
	// We have to re-check if we are still connected to the peer.
	// It does not matter which connection is used to the peer, as the Bitswap
	// sender internally uses whatever connection is available to the peer.
	wt.connManager.lock.Lock()
	defer wt.connManager.lock.Unlock()
	peerEntry, ok = wt.connManager.peers[remotePeer]
	if !ok {
		// Clean up.
		log.Debugf("bitswap connector: no longer connected to peer %s, closing sender", remotePeer)
		_ = sender.Close()
		return
	}
	if peerEntry.bitswapSender != nil {
		// This should rarely happen, but who knows.
		log.Debugf("bitswap connector: already have a sender to peer %s, closing sender", remotePeer)
		_ = sender.Close()
		return
	}

	log.Debugf("bitswap connector: adding new sender to peer %s", remotePeer)
	peerEntry.bitswapSender = sender
	wt.connManager.peers[remotePeer] = peerEntry
}

// Disconnected is called when a connection is closed.
func (wt *BitswapWireTap) Disconnected(_ network.Network, conn network.Conn) {
	remotePeer := conn.RemotePeer()
	log.Debugf("Disconnection event for peer %s, address %s", remotePeer, conn.RemoteMultiaddr())

	wt.connManager.lock.Lock()
	defer wt.connManager.lock.Unlock()
	if _, ok := wt.connManager.allConnections[conn.ID()]; !ok {
		log.Warnf("Missing connection ID %s (Duplicate Disconnection Event or Missing Connection Event)", conn.ID())
		// What now?
	}
	delete(wt.connManager.allConnections, conn.ID())

	peerConn, ok := wt.connManager.peers[remotePeer]
	if !ok {
		log.Warnf("Missing entry for disconnected peer %s", remotePeer)
		// What now?
	} else {
		if _, ok := peerConn.connections[conn.ID()]; !ok {
			log.Warnf("Missing connection entry with ID %s for p %s", conn.ID(), remotePeer)
			// What now?
		}
		delete(peerConn.connections, conn.ID())

		if len(peerConn.connections) == 0 {
			// Clean up Bitswap sender
			if peerConn.bitswapSender != nil {
				_ = peerConn.bitswapSender.Close()
			}

			delete(wt.connManager.peers, remotePeer)
		}
	}

	wt.notifyConnEvent(false, conn)
}

// OpenedStream is called when a stream has been opened.
// We do not use this at the moment.
func (*BitswapWireTap) OpenedStream(_ network.Network, s network.Stream) {
	log.Debugf("Opened stream to peer %s, stream ID %s, direction %s, protocol %s", s.Conn().RemotePeer(), s.ID(), s.Stat().Direction, s.Protocol())
}

// ClosedStream is called when a stream has been closed.
// We do not use this at the moment.
func (*BitswapWireTap) ClosedStream(_ network.Network, s network.Stream) {
	log.Debugf("Closed stream to peer %s, stream ID %s, direction %s, protocol %s", s.Conn().RemotePeer(), s.ID(), s.Stat().Direction, s.Protocol())
}
