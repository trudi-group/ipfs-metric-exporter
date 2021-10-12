package main

import (
	pl "meplugin/metricplugin"

	"github.com/ipfs/go-ipfs/plugin"
)

var Plugins = []plugin.Plugin{
	&pl.MetricExporterPlugin{},
}
