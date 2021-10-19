package metricplugin

import (
	"fmt"
	"strings"
	"time"

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

type BitSwapWireTap struct {
	api        *core.IpfsNode
	gatewayMap map[peer.ID]string
}

func streamsContainKad(streams []network.Stream) bool {
	var containsKad = false
	for _, s := range streams {
		if strings.Contains(string(s.Protocol()), kadDHTString) {
			// fmt.Printf("Protocol.ID contains kad: %s\n", s.Protocol())
			return true
		}
	}

	return containsKad
}

func (bwt *BitSwapWireTap) MainLoop() {
	pollticker := time.NewTicker(pollInterval)

	for {
		// Every time the ticker fires we return
		// (1) The number of DHT clients and servers
		// (2) The agent version of every connected peer
		// ad (1)
		<-pollticker.C
		//fmt.Println("Ticker fired")
		currentConns := bwt.api.PeerHost.Network().Conns()

		var dht_enabled = 0.0
		var dht_disabled = 0.0

		for _, c := range currentConns {
			streams := c.GetStreams()
			if streamsContainKad(streams) {
				dht_enabled++
			} else {
				dht_disabled++
			}
		}

		// Send to prometheus
		dhtEnabledPeers.With(prometheus.Labels{"dht_enabled": "yes"}).Set(dht_enabled)
		dhtEnabledPeers.With(prometheus.Labels{"dht_enabled": "no"}).Set(dht_disabled)

		// ad (2)
		// TODO: Alternatively solve this via the connection events -- if we have the peer store entry then already.
		// It could be the case that the ID protocol has not finished when we receive the connection event
		agentVersionCount.Reset()

		for _, p := range bwt.api.PeerHost.Network().Peers() {
			var av string
			agentVersion, err := bwt.api.PeerHost.Peerstore().Get(p, "AgentVersion")
			if err == nil {
				av = agentVersion.(string)
			}
			agentVersionCount.With(prometheus.Labels{"agent_version": av}).Inc()
		}
	}
}

func (bwt BitSwapWireTap) RecordIfGateway(pid peer.ID) {
	// Get the gateway from the corresponding map
	if val, ok := bwt.gatewayMap[pid]; ok {
		trafficByGateway.With(prometheus.Labels{"gateway": val}).Inc()
	} else {
		trafficByGateway.With(prometheus.Labels{"gateway": "homegrown"}).Inc()
	}
}

// The two functions of the Bitswap notifactions: MessageReceived() and MessageSent()
func (bwt BitSwapWireTap) MessageReceived(pid peer.ID, msg bsmsg.BitSwapMessage) {
	/* 	conns := bwt.api.PeerHost.Network().ConnsToPeer(pid)
	   	// Unpack the multiaddresses
	   	var mas []ma.Multiaddr
	   	for _, c := range conns {
	   		mas = append(mas, c.RemoteMultiaddr())
	   	}
	   	fmt.Printf("Received msg from %s with multiaddrs: %s \n", pid, mas) */

	bwt.RecordIfGateway(pid)

	// Process incoming bsmsg for prometheus metrics. In particular we want to
	// extract whether a received message originated from a gateway or not

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
