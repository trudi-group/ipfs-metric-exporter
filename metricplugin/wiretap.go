package metricplugin

import (
	"fmt"

	bsmsg "github.com/ipfs/go-bitswap/message"
	core "github.com/ipfs/go-ipfs/core"
	"github.com/libp2p/go-libp2p-core/network"
	peer "github.com/libp2p/go-libp2p-core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

type BitSwapWireTap struct {
	api *core.IpfsNode
}

func (bwt BitSwapWireTap) MessageReceived(pid peer.ID, msg bsmsg.BitSwapMessage) {
	conns := bwt.api.PeerHost.Network().ConnsToPeer(pid)
	// Unpack the multiaddresses
	var mas []ma.Multiaddr
	for _, c := range conns {
		mas = append(mas, c.RemoteMultiaddr())
	}
	fmt.Printf("Received msg from %s with multiaddrs: %s \n", pid, mas)
}

func (BitSwapWireTap) MessageSent(pid peer.ID, msg bsmsg.BitSwapMessage) {
	// NOP
}

// Implement the network notifee interface
func (BitSwapWireTap) Listen(nw network.Network, ma ma.Multiaddr) {
	// NOP
}

func (BitSwapWireTap) ListenClose(nw network.Network, ma ma.Multiaddr) {
	// NOP
}

func (BitSwapWireTap) Connected(nw network.Network, conn network.Conn) {
	fmt.Printf("Connection event for PID: %s, Addr: %s\n", conn.RemotePeer(), conn.RemoteMultiaddr())
}

func (BitSwapWireTap) Disconnected(nw network.Network, conn network.Conn) {
	fmt.Printf("Disconnection event for PID: %s, Addr: %s\n", conn.RemotePeer(), conn.RemoteMultiaddr())
}

func (BitSwapWireTap) OpenedStream(nw network.Network, s network.Stream) {
	// NOP
}

func (BitSwapWireTap) ClosedStream(nw network.Network, s network.Stream) {
	// NOP
}
