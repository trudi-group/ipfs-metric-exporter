package metricplugin

import (
	"fmt"

	bsmsg "github.com/ipfs/go-bitswap/message"
	core "github.com/ipfs/go-ipfs/core"
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
	// Do nothing
}
