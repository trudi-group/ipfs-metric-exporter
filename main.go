// Package main implements a kubo (go-ipfs) plugin to export additional metrics
// and Bitswap traffic from an IPFS node.
package main

import (
	pl "github.com/trudi-group/ipfs-metric-exporter/metricplugin"

	"github.com/ipfs/kubo/plugin"
)

// Plugins lists implementations of the `plugin.Plugin` interface exported by
// this IPFS plugin.
var Plugins = []plugin.Plugin{
	&pl.MetricExporterPlugin{},
}
