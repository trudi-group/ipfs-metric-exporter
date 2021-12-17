package metricplugin

import (
	"github.com/prometheus/client_golang/prometheus"
)

var supportedProtocolsAmongConnectedPeers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plugin_metric_export_peers_supported_protocols",
	Help: "Sum of supported protocols over all currently connected peers, as reported by the IPFS node.",
},
	[]string{"protocol"},
)

var agentVersionCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plugin_metric_export_peers_by_agent_version",
	Help: "Number of currently connected peers, distinguished by their agent_version.",
},
	[]string{"agent_version"},
)

var streamCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plugin_metric_export_open_streams",
	Help: "Number of currently open streams, by protocol and direction, as reported by the IPFS node.",
},
	[]string{"protocol", "direction"})

var wiretapBitswapSenderCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "plugin_metric_export_bitswap_senders",
	Help: "Number of available Bitswap senders to use to send Bitswap messages.",
})

var wiretapPeerCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "plugin_metric_export_wiretap_peers",
	Help: "Number of connected peers tracked via the wiretap, based on connection events reported by the IPFS node.",
})

var wiretapConnectionCount = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "plugin_metric_export_wiretap_connections",
	Help: "Number of connections tracked via the wiretap, based on connection events reported by the IPFS node.",
})
