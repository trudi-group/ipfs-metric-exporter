package metricplugin

import (
	"fmt"

	bsmsg "github.com/ipfs/go-bitswap/message"
	peer "github.com/libp2p/go-libp2p-core/peer"
)

type BitSwapWireTap struct{}

func (BitSwapWireTap) MessageReceived(pid peer.ID, msg bsmsg.BitSwapMessage) {
	fmt.Printf("Received msg from %s\n", pid)
}

func (BitSwapWireTap) MessageSent(pid peer.ID, msg bsmsg.BitSwapMessage) {
	// Do nothing
}
