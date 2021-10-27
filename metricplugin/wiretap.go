package metricplugin

import (
	"fmt"

	"github.com/ipfs/go-bitswap"
	bsmsg "github.com/ipfs/go-bitswap/message"
	core "github.com/ipfs/go-ipfs/core"
	"github.com/libp2p/go-libp2p-core/network"
	peer "github.com/libp2p/go-libp2p-core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	kadDHTString = "/kad/"
)

var _ bitswap.WireTap = (*BitswapWireTap)(nil)

// BitswapWireTap implements the `WireTap` interface to log Bitswap messages.
type BitswapWireTap struct {
	api        *core.IpfsNode
	gatewayMap map[peer.ID]string
}

func (wt *BitswapWireTap) prometheusRecordMessageReceived(pid peer.ID) {
	// Get the gateway from the corresponding map
	if val, ok := wt.gatewayMap[pid]; ok {
		trafficByGateway.With(prometheus.Labels{"gateway": val}).Inc()
	} else {
		trafficByGateway.With(prometheus.Labels{"gateway": "homegrown"}).Inc()
	}
}

// MessageReceived is called on incoming Bitswap messages.
func (wt *BitswapWireTap) MessageReceived(pid peer.ID, _ bsmsg.BitSwapMessage) {
	/* 	conns := wt.api.PeerHost.Network().ConnsToPeer(pid)
	   	// Unpack the multiaddresses
	   	var mas []ma.Multiaddr
	   	for _, c := range conns {
	   		mas = append(mas, c.RemoteMultiaddr())
	   	}
	   	fmt.Printf("Received msg from %s with multiaddrs: %s \n", pid, mas) */

	wt.prometheusRecordMessageReceived(pid)

	// Process incoming bsmsg for prometheus metrics. In particular we want to
	// extract whether a received message originated from a gateway or not
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

// Connected is called when a connection is opened.
func (*BitswapWireTap) Connected(_ network.Network, conn network.Conn) {
	fmt.Printf("Connection event for PID: %s, Addr: %s\n", conn.RemotePeer(), conn.RemoteMultiaddr())
}

// Disconnected is called when a connection is closed.
func (*BitswapWireTap) Disconnected(_ network.Network, conn network.Conn) {
	fmt.Printf("Disconnection event for PID: %s, Addr: %s\n", conn.RemotePeer(), conn.RemoteMultiaddr())
}

// OpenedStream is called when a stream has been opened.
// We do not use this at the moment.
func (*BitswapWireTap) OpenedStream(network.Network, network.Stream) {}

// ClosedStream is called when a stream has been closed.
// We do not use this at the moment.
func (*BitswapWireTap) ClosedStream(network.Network, network.Stream) {}
