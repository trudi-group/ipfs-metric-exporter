package metricplugin

import (
	"fmt"

	bs "github.com/ipfs/go-bitswap"
	core "github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/plugin"
)

type MetricExporterPlugin struct {
	api      *core.IpfsNode
	bsEngine *bs.Bitswap
}

var _ plugin.PluginDaemonInternal = (*MetricExporterPlugin)(nil)

// We have to satisfy the plugin.Plugin interface, so we need a Name(), Version(), and Init()
func (*MetricExporterPlugin) Name() string {
	return "metric-export-plugin"
}

func (*MetricExporterPlugin) Version() string {
	return "0.0.1"
}

func (*MetricExporterPlugin) Init(env *plugin.Environment) error {
	return nil
}

func (mep *MetricExporterPlugin) Start(ipfsInstance *core.IpfsNode) error {
	fmt.Println("Metric Export Plugin started")
	mep.api = ipfsInstance

	bitswapEngine, ok := mep.api.Exchange.(*bs.Bitswap)
	if !ok {
		panic("Could not get BS Object.")
	}
	mep.bsEngine = bitswapEngine

	// Create a wiretap instance
	bswt := BitSwapWireTap{}
	optFunc := bs.EnableWireTap(bswt)
	optFunc(bitswapEngine)
	// go cp.forkToBackground()
	return nil
}
