package metricplugin

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p-core/network"
	"github.com/pkg/errors"

	bs "github.com/ipfs/go-bitswap"
	core "github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/plugin"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	pollInterval = 10 * time.Second
)

// MetricExporterPlugin holds the state of the metrics exporter plugin.
type MetricExporterPlugin struct {
	api      *core.IpfsNode
	bsEngine *bs.Bitswap
	conf     Config
}

// Config contains all values configurated via the standard IPFS config section on plugins.
type Config struct {
	QueryPeerListIntervalSeconds int `json:"QueryPeerListIntervalSeconds"`
}

var gwFilePath = "gateway_list.csv"

var _ plugin.PluginDaemonInternal = (*MetricExporterPlugin)(nil)

// Name returns the name of this plugin.
// This is part of the `plugin.Plugin` interface.
func (*MetricExporterPlugin) Name() string {
	return "metric-export-plugin"
}

// Version returns the version of this plugin.
// This is part of the `plugin.Plugin` interface.
func (*MetricExporterPlugin) Version() string {
	return "0.0.1"
}

// Init initializes this plugin with the given environment.
// This is part of the `plugin.Plugin` interface.
func (mep *MetricExporterPlugin) Init(env *plugin.Environment) error {
	// Populate the config
	jconf, err := json.Marshal(env.Config)
	if err != nil {
		return errors.Wrap(err, "unable to marshal plugin config to JSON")
	}

	var pConf Config
	err = json.Unmarshal(jconf, &pConf)
	if err != nil {
		return errors.Wrap(err, "unable to unmarshal plugin config from JSON")
	}

	mep.conf = pConf

	return nil
}

// Start starts this plugin.
// This is not run in a separate goroutine and should not block.
func (mep *MetricExporterPlugin) Start(ipfsInstance *core.IpfsNode) error {
	fmt.Println("Metric Export Plugin started")

	// Register metrics
	prometheus.MustRegister(trafficByGateway)
	prometheus.MustRegister(dhtEnabledPeers)
	prometheus.MustRegister(agentVersionCount)

	// Get the bitswap instance from the interface
	mep.api = ipfsInstance

	bitswapEngine, ok := mep.api.Exchange.(*bs.Bitswap)
	if !ok {
		return errors.New("could not get Bitswap implementation")
	}
	mep.bsEngine = bitswapEngine

	// Read gateway list from file.
	gwMap, err := readGatewayListFromFile(gwFilePath)
	if err != nil {
		return errors.Wrap(err, "unable to read list of gateways")
	}

	// Create a wiretap instance & Subscribe to notifications in Bitswap & network.Network
	bswt := &BitswapWireTap{
		api:        ipfsInstance,
		gatewayMap: gwMap,
	}
	bs.EnableWireTap(bswt)(bitswapEngine)

	// TODO: For some reason this breaks everything and leads to a deadlock
	// ipfsInstance.PeerHost.Network().Notify(bswt)

	// Periodically populate Prometheus with metrics about peer counts.
	go mep.populatePromPeerCountLoop(time.Duration(mep.conf.QueryPeerListIntervalSeconds) * time.Second)

	return nil
}

func streamsContainKad(streams []network.Stream) bool {
	for _, s := range streams {
		if strings.Contains(string(s.Protocol()), kadDHTString) {
			// fmt.Printf("Protocol.ID contains kad: %s\n", s.Protocol())
			return true
		}
	}

	return false
}

func (mep *MetricExporterPlugin) populatePromPeerCountLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)

	for {
		// Every time the ticker fires we return
		// (1) The number of DHT clients and servers
		// (2) The agent version of every connected peer
		// ad (1)
		<-ticker.C
		// fmt.Println("Ticker fired")
		currentConns := mep.api.PeerHost.Network().Conns()

		dhtEnabledCount := 0
		dhtDisabledCount := 0

		for _, c := range currentConns {
			streams := c.GetStreams()
			if streamsContainKad(streams) {
				dhtEnabledCount++
			} else {
				dhtDisabledCount++
			}
		}

		// Populate prometheus
		dhtEnabledPeers.With(prometheus.Labels{"dht_enabled": "yes"}).Set(float64(dhtEnabledCount))
		dhtEnabledPeers.With(prometheus.Labels{"dht_enabled": "no"}).Set(float64(dhtDisabledCount))

		// ad (2)
		// One could alternatively solve this via the connection events -- if we have the peer store entry then already.
		// It could be the case that the ID protocol has not finished when we receive the connection event.
		// -> After talking to mxinden it is definitely the case that the ID protocol will not have finished
		agentVersionCount.Reset()
		connectedPeers := mep.api.PeerHost.Network().Peers()

		for _, p := range connectedPeers {
			agentVersion, err := mep.api.PeerHost.Peerstore().Get(p, "AgentVersion")
			if err != nil {
				continue
			}
			av := agentVersion.(string)

			// TODO this is potentially slow. We could pre-compute the counts.
			agentVersionCount.With(prometheus.Labels{"agent_version": av}).Inc()
		}
	}
}

func readGatewayListFromFile(path string) (map[peer.ID]string, error) {
	gwFile, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to open gateway file %s", path)
	}
	defer gwFile.Close()

	csvReader := csv.NewReader(gwFile)

	// Skip the header.
	_, err = csvReader.Read()
	if err != nil {
		return nil, errors.Wrap(err, "unable to read CSV record")
	}

	// Read actual entries.
	gatewayMap := make(map[peer.ID]string)
	for {
		line, err := csvReader.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, errors.Wrap(err, "unable to read CSV record")
		}
		if len(line) != 2 {
			return nil, fmt.Errorf("invalid number of fields in gateway CSV, expected 2, got %d", len(line))
		}

		// Decode the record:
		// First field is the Peer ID, second field is the gateway operator.

		peerID, err := peer.Decode(line[0])
		if err != nil {
			return nil, errors.Wrap(err, "unable to decode peer ID")
		}
		if _, ok := gatewayMap[peerID]; ok {
			return nil, fmt.Errorf("duplicate peer ID %s in gateway CSV", peerID)
		}

		operator := strings.ToLower(line[1])

		gatewayMap[peerID] = operator
	}

	return gatewayMap, nil
}
