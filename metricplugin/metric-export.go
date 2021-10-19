package metricplugin

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"time"

	bs "github.com/ipfs/go-bitswap"
	core "github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/plugin"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	pollInterval = 10 * time.Second
)

type MetricExporterPlugin struct {
	api      *core.IpfsNode
	bsEngine *bs.Bitswap
}

var gwFilePath = "gateway_list.csv"

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

	// Register metrics
	prometheus.Register(trafficByGateway)
	prometheus.Register(dhtEnabledPeers)
	prometheus.Register(agentVersionCount)

	// Get the bitswap instance from the interface
	mep.api = ipfsInstance

	bitswapEngine, ok := mep.api.Exchange.(*bs.Bitswap)
	if !ok {
		panic("Could not get BS Object.")
	}
	mep.bsEngine = bitswapEngine

	// Create a wiretap instance & Subscribe to notifactions in Bitswap & network.Network

	gwMap := ReadGWListFromFile(gwFilePath)
	bswt := BitSwapWireTap{
		api:        ipfsInstance,
		gatewayMap: gwMap,
	}

	optFunc := bs.EnableWireTap(bswt)
	optFunc(bitswapEngine)

	// TODO: For some reason this breaks everything and leads to a deadlock
	//ipfsInstance.PeerHost.Network().Notify(bswt)

	// Fork to background
	go bswt.MainLoop()
	return nil
}

func ReadGWListFromFile(path string) map[peer.ID]string {
	gwFile, err := os.Open(path)
	defer gwFile.Close()

	if err != nil {
		panic(err)
	}

	var lines []string
	gatewayMap := make(map[peer.ID]string)
	scanner := bufio.NewScanner(gwFile)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	// Skip the header
	for _, l := range lines[1:] {
		sline := strings.Split(l, ",")
		// First entry is PID, second entry is gw operator
		pid, err := peer.Decode(strings.Trim(sline[0], "\""))
		if err != nil {
			fmt.Printf("Error decoding peer ID in gw list, %s\n", err)
			continue
		}
		gatewayMap[pid] = strings.Trim(sline[1], "\"")
	}

	return gatewayMap
}
