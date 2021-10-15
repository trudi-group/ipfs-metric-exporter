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
