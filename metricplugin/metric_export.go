package metricplugin

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/libp2p/go-libp2p-core/network"
	"github.com/pkg/errors"

	bs "github.com/ipfs/go-bitswap"
	core "github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/plugin"
	logging "github.com/ipfs/go-log"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// The default interval at which to populate prometheus with peer list
	// metrics.
	defaultQueryPeerListIntervalSeconds int = 10
)

var (
	_ PluginAPI                   = (*MetricExporterPlugin)(nil)
	_ plugin.PluginDaemonInternal = (*MetricExporterPlugin)(nil)

	// Implementing io.Closer causes the IPFS daemon to gracefully shut down
	// our plugin.
	_ io.Closer = (*MetricExporterPlugin)(nil)
)

var log = logging.Logger("metric-export")

// MetricExporterPlugin holds the state of the metrics exporter plugin.
type MetricExporterPlugin struct {
	api  *core.IpfsNode
	conf Config

	wiretap *BitswapWireTap

	// If not nil, a reference to the TCP server.
	// Used for clean shutdown.
	tcpServer *tcpServer
}

// Subscribe implements PluginAPI.
func (mep *MetricExporterPlugin) Subscribe(subscriber EventSubscriber) error {
	// This is racy, but probably fine
	if mep.wiretap != nil {
		return mep.wiretap.subscribe(subscriber)
	}
	panic("Subscribe called but Bitswap Wiretap not set up")
}

// Unsubscribe implements PluginAPI.
func (mep *MetricExporterPlugin) Unsubscribe(subscriber EventSubscriber) {
	// This is racy, but probably fine
	if mep.wiretap != nil {
		mep.wiretap.unsubscribe(subscriber)
	}
}

// Ping implements PluginAPI.
func (*MetricExporterPlugin) Ping() {}

// Config contains all values configured via the standard IPFS config section on plugins.
type Config struct {
	// The interval at which to populate prometheus with statistics about
	// currently connected peers.
	// Set to zero to deactivate.
	QueryPeerListIntervalSeconds int `json:"QueryPeerListIntervalSeconds"`

	// Configuration of the TCP server config.
	// If this is nil, the TCP logging will not be activated.
	TCPServerConfig *TCPServerConfig `json:"TCPServerConfig"`
}

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
	jConf, err := json.Marshal(env.Config)
	if err != nil {
		return errors.Wrap(err, "unable to marshal plugin config to JSON")
	}

	var pConf Config
	err = json.Unmarshal(jConf, &pConf)
	if err != nil {
		return errors.Wrap(err, "unable to unmarshal plugin config from JSON")
	}

	if pConf.QueryPeerListIntervalSeconds < 0 {
		log.Errorf("invalid QueryPeerListIntervalSeconds, using default (%d)", defaultQueryPeerListIntervalSeconds)
		pConf.QueryPeerListIntervalSeconds = defaultQueryPeerListIntervalSeconds
	} else if pConf.QueryPeerListIntervalSeconds == 0 {
		log.Info("QueryPeerListIntervalSeconds is zero, these metrics will not be published to prometheus.")
	}

	if pConf.TCPServerConfig == nil {
		log.Info("TCPServerConfig is nil, TCP logging disabled.")
	}

	mep.conf = pConf

	return nil
}

// Start starts this plugin.
// This is not run in a separate goroutine and should not block.
func (mep *MetricExporterPlugin) Start(ipfsInstance *core.IpfsNode) error {
	if !ipfsInstance.IsDaemon {
		// Do not start in client mode.
		// This should never happen.
		panic("metric exporter plugin started with IPFS not in daemon mode")
	}

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

	// Create a wiretap instance & subscribe to notifications in Bitswap &
	// network.Network.
	// We need to start this before we start the TCP server, so that clients can
	// subscribe immediately without fail or races.
	mep.wiretap = &BitswapWireTap{
		api: ipfsInstance,
	}
	bs.EnableWireTap(mep.wiretap)(bitswapEngine)

	// Subscribe to network events.
	ipfsInstance.PeerHost.Network().Notify(mep.wiretap)

	// Periodically populate Prometheus with metrics about peer counts.
	if mep.conf.QueryPeerListIntervalSeconds != 0 {
		go mep.populatePromPeerCountLoop(time.Duration(mep.conf.QueryPeerListIntervalSeconds) * time.Second)
	}

	if mep.conf.TCPServerConfig != nil {
		tcpServer, err := newTCPServer(mep, *mep.conf.TCPServerConfig)
		if err != nil {
			return errors.Wrap(err, "unable to start TCP server")
		}
		mep.tcpServer = tcpServer

		go mep.tcpServer.listenAndServe()
	}

	fmt.Println("Metric Export Plugin started")
	return nil
}

// Close implements io.Closer.
// This is called to cleanly shut down the plugin when the daemon exits.
func (mep *MetricExporterPlugin) Close() error {
	// This is potentially racy, but probably not in our case.
	if mep.tcpServer != nil {
		mep.tcpServer.Shutdown()
	}
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
