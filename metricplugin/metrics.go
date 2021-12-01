package metricplugin

import (
	"github.com/prometheus/client_golang/prometheus"
)

var trafficByGateway = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "plugin_metric_export_recv_bsmsg",
	Help: "The total number of received Bitswap messages differentiated by gateways and non-gateways.",
},
	[]string{"gateway"},
)

var supportedProtocolsAmongConnectedPeers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plugin_metric_export_peers_supported_protocols",
	Help: "Sum of supported protocols over all currently connected peers, as reported by the IPFS node.",
},
	[]string{"protocol"},
)

var agentVersionCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plugin_metric_export_peers_by_agent_version",
	Help: "Number of currently connected peers, distinguished by their agent_version",
},
	[]string{"agent_version"},
)

var streamCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plugin_metric_export_open_streams",
	Help: "Number of currently open streams, by protocol and direction, as reported by the IPFS node.",
},
	[]string{"protocol", "direction"})
