package metricplugin

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"sync"
	"time"
	"unsafe"

	bs "github.com/ipfs/go-bitswap"
	bsnet "github.com/ipfs/go-bitswap/network"
	"github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/plugin"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// The default interval at which to populate prometheus with peer list
	// metrics.
	defaultQueryPeerListIntervalSeconds int = 10
	// The default number of most-seen agent versions that are reported to prometheus.
	defaultAgentVersionCutOff int = 20
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

	// Lifecycle management
	closing     chan struct{}
	closingLock sync.Mutex
	wg          sync.WaitGroup
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
	// currently connected peers, streams, connections, etc.
	PopulatePrometheusInterval int `json:"PopulatePrometheusInterval"`

	// The number of top N agent versions that are reported to prometheus.
	// AVs are somewhat arbitrarily chosen strings and clutter prometheus.
	AgentVersionCutOff int `json:"AgentVersionCutOff"`

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
	return "0.2.0"
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

	if pConf.PopulatePrometheusInterval <= 0 {
		log.Errorf("invalid PopulatePrometheusInterval, using default (%d)", defaultQueryPeerListIntervalSeconds)
		pConf.PopulatePrometheusInterval = defaultQueryPeerListIntervalSeconds
	}

	if pConf.AgentVersionCutOff <= 0 {
		log.Errorf("invalid AgentVersionCutOff, using default (%d)", defaultAgentVersionCutOff)
		pConf.AgentVersionCutOff = defaultAgentVersionCutOff
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
	mep.api = ipfsInstance
	mep.closing = make(chan struct{})

	// Register metrics.
	prometheus.MustRegister(supportedProtocolsAmongConnectedPeers)
	prometheus.MustRegister(agentVersionCount)
	prometheus.MustRegister(streamCount)
	prometheus.MustRegister(wiretapBitswapSenderCount)
	prometheus.MustRegister(wiretapPeerCount)
	prometheus.MustRegister(wiretapConnectionCount)

	// Get the bitswap instance from the interface.
	bitswapEngine, ok := mep.api.Exchange.(*bs.Bitswap)
	if !ok {
		return errors.New("could not get Bitswap implementation")
	}

	// Get the bsnet.BitSwapNetwork implementation used in running Bitswap
	// instance.
	// This will break if the structure of the bs.Bitswap struct changes.
	// Hopefully that doesn't happen too often...
	// See https://stackoverflow.com/a/60598827
	bsnetImpl, ok := (getUnexportedField(reflect.ValueOf(bitswapEngine).Elem().FieldByName("network"))).(bsnet.BitSwapNetwork)
	if !ok {
		return errors.New("could not get Bitswap network implementation")
	}

	// Create a wiretap instance & subscribe to notifications in Bitswap &
	// network.Network.
	// We need to start this before we start the TCP server, so that clients can
	// subscribe immediately without fail or races.
	mep.wiretap = NewWiretap(ipfsInstance, bsnetImpl)
	bs.EnableWireTap(mep.wiretap)(bitswapEngine)

	// Subscribe to network events.
	ipfsInstance.PeerHost.Network().Notify(mep.wiretap)

	// Periodically populate Prometheus with metrics about peer counts.
	if mep.conf.PopulatePrometheusInterval != 0 {
		mep.wg.Add(1)
		go mep.populatePrometheus(time.Duration(mep.conf.PopulatePrometheusInterval) * time.Second)
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

// Reflect magic.
// See https://stackoverflow.com/a/60598827.
func getUnexportedField(field reflect.Value) interface{} {
	return reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem().Interface()
}

// Close implements io.Closer.
// This is called to cleanly shut down the plugin when the daemon exits.
// This is idempotent.
func (mep *MetricExporterPlugin) Close() error {
	// This is potentially racy, but probably not in our case.
	if mep.tcpServer != nil {
		mep.tcpServer.Shutdown()
	}
	if mep.wiretap != nil {
		mep.wiretap.Shutdown()
	}

	mep.closingLock.Lock()
	select {
	case <-mep.closing:
	// Already closed/closing
	default:
		close(mep.closing)
	}
	mep.closingLock.Unlock()

	mep.wg.Wait()

	return nil
}

func (mep *MetricExporterPlugin) populatePrometheus(interval time.Duration) {
	defer mep.wg.Done()
	ticker := time.NewTicker(interval)

	for {
		// Every time the ticker fires we compute
		// (1) The supported protocols among connected peers
		// (2) The agent version of every connected peer
		// (3) The number of streams by protocol and direction
		select {
		case <-ticker.C:
		case <-mep.closing:
			log.Debug("populatePrometheus loop exiting")
			return
		}

		// We ask the IPFS node for these just once and then use them to
		// calculate various things for prometheus.
		// That way, the metrics should be consistent for one point in time.
		currentConns := mep.api.PeerHost.Network().Conns()
		currentPeers := mep.api.PeerHost.Network().Peers()

		// ad (1): Sum supported protocols over connected peers.
		before := time.Now()
		protocolCounts := make(map[string]int)

		for _, peerID := range currentPeers {
			// This returns a slice of strings instead of a slice of protocol.ID
			// I don't know why...
			protocols, err := mep.api.Peerstore.GetProtocols(peerID)
			if err != nil {
				log.Warnf("populatePrometheus: unable to get protocols for peer %s: %s", peerID, err)
				continue
			}
			for _, protocolID := range protocols {
				protocolCounts[protocolID] = protocolCounts[protocolID] + 1
			}
		}
		elapsed := time.Since(before)

		log.Debugf("populatePrometheus: took %s to count supported protocols among %d peers: %+v", elapsed, len(currentPeers), protocolCounts)
		supportedProtocolsAmongConnectedPeers.Reset()
		for protocolID, count := range protocolCounts {
			supportedProtocolsAmongConnectedPeers.With(prometheus.Labels{"protocol": protocolID}).Set(float64(count))
		}

		// ad (2): Count agent versions of connected peers.
		// One could alternatively solve this via the connection events -- if we
		// have the peer store entry then already.
		// It could be the case that the ID protocol has not finished when we
		// receive the connection event.
		// -> After talking to mxinden it is definitely the case that the ID
		// protocol will not have finished
		before = time.Now()
		countsByAgentVersion := make(map[string]int)

		for _, p := range currentPeers {
			agentVersion, err := mep.api.PeerHost.Peerstore().Get(p, "AgentVersion")
			if err != nil {
				continue
			}
			av := agentVersion.(string)
			countsByAgentVersion[av] = countsByAgentVersion[av] + 1
		}
		// Sort the agent versions in decreasing order
		sortedAgentVersions := make([]string, 0, len(countsByAgentVersion))
		for av := range countsByAgentVersion {
			sortedAgentVersions = append(sortedAgentVersions, av)
		}
		sort.Slice(sortedAgentVersions, func(i, j int) bool {
			// This expects a Less() function, but since we want descending order we use ">"
			return countsByAgentVersion[sortedAgentVersions[i]] > countsByAgentVersion[sortedAgentVersions[j]]
		})
		elapsed = time.Since(before)

		log.Debugf("populatePrometheus: took %s to calculate agent version counts %+v", elapsed, countsByAgentVersion)
		agentVersionCount.Reset()
		maxIndex := mep.conf.AgentVersionCutOff
		// There is no math.Min for integers and typecasting two ints -> float64 and the result -> int looks super confusing.
		if mep.conf.AgentVersionCutOff >= len(sortedAgentVersions) {
			maxIndex = len(sortedAgentVersions)
		}

		for _, agentVersion := range sortedAgentVersions[:maxIndex] {
			agentVersionCount.With(prometheus.Labels{
				"agent_version": agentVersion,
			}).Set(float64(countsByAgentVersion[agentVersion]))
		}
		// Add all other agent versions to a "other" category
		var avSum int
		for _, av := range sortedAgentVersions[maxIndex:] {
			avSum += countsByAgentVersion[av]
		}
		agentVersionCount.With(prometheus.Labels{
			"agent_version": "others",
		}).Set(float64(avSum))

		// ad (3): Count streams by protocol and direction.
		before = time.Now()
		countsByProtocolAndDirection := make(map[protocol.ID]struct {
			inbound  int
			outbound int
			unknown  int
		})

		for _, conn := range currentConns {
			streams := conn.GetStreams()
			for _, stream := range streams {
				protocolID := stream.Protocol()
				count := countsByProtocolAndDirection[protocolID]

				direction := stream.Stat().Direction
				if direction == network.DirInbound {
					count.inbound++
				} else if direction == network.DirOutbound {
					count.outbound++
				} else if direction == network.DirUnknown {
					count.unknown++
				} else {
					panic(fmt.Sprintf("got invalid stream direction %d", direction))
				}

				countsByProtocolAndDirection[protocolID] = count
			}
		}
		elapsed = time.Since(before)

		log.Debugf("populatePrometheus: took %s to calculate stream counts %+v", elapsed, countsByProtocolAndDirection)
		streamCount.Reset()
		for protocolID, counts := range countsByProtocolAndDirection {
			if counts.inbound != 0 {
				streamCount.With(prometheus.Labels{
					"protocol":  string(protocolID),
					"direction": "inbound",
				}).Set(float64(counts.inbound))
			}

			if counts.outbound != 0 {
				streamCount.With(prometheus.Labels{
					"protocol":  string(protocolID),
					"direction": "outbound",
				}).Set(float64(counts.outbound))
			}

			if counts.unknown != 0 {
				streamCount.With(prometheus.Labels{
					"protocol":  string(protocolID),
					"direction": "unknown",
				}).Set(float64(counts.unknown))
			}
		}
	}
}
