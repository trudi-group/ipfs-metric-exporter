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

var dhtEnabledPeers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plugin_metric_export_peers_by_dht_enabled",
	Help: "Number of currently connected peers, distinguished by whether they speak the DHT protocol or not.",
},
	[]string{"dht_enabled"},
)

var agentVersionCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "plugin_metric_export_peers_by_agent_version",
	Help: "Number of currently connected peers, distinguished by their agent_version",
},
	[]string{"agent_version"},
)
