package main

import (
	pl "meplugin/metricplugin"

	"github.com/ipfs/kubo/plugin"
)

// Plugins lists implementations of the `plugin.Plugin` interface exported by
// this IPFS plugin.
var Plugins = []plugin.Plugin{
	&pl.MetricExporterPlugin{},
}
