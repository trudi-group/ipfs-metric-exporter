package metricplugin

import (
	"sync"
	"time"

	"github.com/ipfs/go-bitswap"
	bsmsg "github.com/ipfs/go-bitswap/message"
	core "github.com/ipfs/go-ipfs/core"
	"github.com/libp2p/go-libp2p-core/network"
	peer "github.com/libp2p/go-libp2p-core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

const (
	kadDHTString = "/kad/"
)

var (
	_ bitswap.WireTap  = (*BitswapWireTap)(nil)
	_ network.Notifiee = (*BitswapWireTap)(nil)
)

// BitswapWireTap implements the `WireTap` interface to log Bitswap messages.
type BitswapWireTap struct {
	api         *core.IpfsNode
	subscribers subscribers
}

type subscribers struct {
	subscribers []EventSubscriber
	lock        sync.RWMutex
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
	msgToLog := BitswapMessage{
		WantlistEntries: msg.Wantlist(),
		FullWantList:    msg.Full(),
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
	log.Debugf("Connection event for PID: %s, Addr: %s\n", conn.RemotePeer(), conn.RemoteMultiaddr())

	wt.notifyConnEvent(true, conn)
}

// Disconnected is called when a connection is closed.
func (wt *BitswapWireTap) Disconnected(_ network.Network, conn network.Conn) {
	log.Debugf("Disconnection event for PID: %s, Addr: %s\n", conn.RemotePeer(), conn.RemoteMultiaddr())

	wt.notifyConnEvent(false, conn)
}

// OpenedStream is called when a stream has been opened.
// We do not use this at the moment.
func (*BitswapWireTap) OpenedStream(network.Network, network.Stream) {}

// ClosedStream is called when a stream has been closed.
// We do not use this at the moment.
func (*BitswapWireTap) ClosedStream(network.Network, network.Stream) {}
