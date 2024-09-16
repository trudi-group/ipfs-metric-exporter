package metricplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"
	"unsafe"

	bs "github.com/ipfs/boxo/bitswap"
	bsnet "github.com/ipfs/boxo/bitswap/network"
	cid "github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log"
	"github.com/ipfs/kubo/core"
	"github.com/ipfs/kubo/plugin"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peerstore"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/fx"
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
	_ plugin.PluginFx             = (*MetricExporterPlugin)(nil)

	// Implementing io.Closer causes the IPFS daemon to gracefully shut down
	// our plugin.
	_ io.Closer = (*MetricExporterPlugin)(nil)

	// ErrBitswapProbeDisabled is returned for Bitswap probing API calls if
	// the Bitswap probe is disabled via the plugin's configuration.
	ErrBitswapProbeDisabled = errors.New("Bitswap probe not enabled")
)

var log = logging.Logger("metric-export")

// MetricExporterPlugin holds the state of the metrics exporter plugin.
type MetricExporterPlugin struct {
	api  *core.IpfsNode
	conf Config

	bitswapDiscoveryProbe *BitswapDiscoveryProbe

	// A reference to the HTTP server, used for clean shutdown.
	httpServer *httpServer

	// Lifecycle management
	closing     chan struct{}
	closingLock sync.Mutex
	wg          sync.WaitGroup
}

// BroadcastBitswapWant implements RPCAPI.
func (mep *MetricExporterPlugin) BroadcastBitswapWant(cids []cid.Cid) ([]BroadcastWantStatus, error) {
	if mep.bitswapDiscoveryProbe == nil {
		return nil, ErrBitswapProbeDisabled
	}
	return mep.bitswapDiscoveryProbe.broadcastBitswapWant(cids), nil
}

// BroadcastBitswapCancel implements RPCAPI.
func (mep *MetricExporterPlugin) BroadcastBitswapCancel(cids []cid.Cid) ([]BroadcastCancelStatus, error) {
	if mep.bitswapDiscoveryProbe == nil {
		return nil, ErrBitswapProbeDisabled
	}
	return mep.bitswapDiscoveryProbe.broadcastBitswapCancel(cids), nil
}

// BroadcastBitswapWantCancel implements RPCAPI.
func (mep *MetricExporterPlugin) BroadcastBitswapWantCancel(cids []cid.Cid, secondsBetween uint) ([]BroadcastWantCancelStatus, error) {
	if mep.bitswapDiscoveryProbe == nil {
		return nil, ErrBitswapProbeDisabled
	}
	return mep.bitswapDiscoveryProbe.broadcastBitswapWantCancel(cids, secondsBetween), nil
}

// Ping implements RPCAPI.
func (*MetricExporterPlugin) Ping() {}

// SamplePeerMetadata implements RPCAPI.
func (mep *MetricExporterPlugin) SamplePeerMetadata(onlyConnected bool) ([]PeerMetadata, int) {
	// Get number of connections and peers in proximity,
	// to have somewhat-consistent values.
	peers := mep.api.Peerstore.Peers()
	numConns := len(mep.api.PeerHost.Network().Conns())
	metadata := make([]PeerMetadata, 0, len(peers))
	log.Debugf("SamplePeerMetadata: have information about %d peers, %d connections", len(peers), numConns)

	for _, p := range peers {
		pmd := PeerMetadata{ID: p}
		pmd.Multiaddrs = mep.api.Peerstore.Addrs(p)
		pmd.Connectedness = mep.api.PeerHost.Network().Connectedness(p)

		if onlyConnected && pmd.Connectedness != network.Connected {
			// Skip
			continue
		}

		if pmd.Connectedness == network.Connected {
			conns := mep.api.PeerHost.Network().ConnsToPeer(p)
			for _, con := range conns {
				pmd.ConnectedMultiaddrs = append(pmd.ConnectedMultiaddrs, con.RemoteMultiaddr())
			}
		}

		protocols, _ := mep.api.Peerstore.GetProtocols(p)
		if len(protocols) != 0 {
			pmd.Protocols = protocols
		}

		ver, err := mep.api.Peerstore.Get(p, "AgentVersion")
		if err != nil {
			log.Debugf("unable to get AgentVersion for peer %s: %s", p, err)
			if err == peerstore.ErrNotFound {
				s := "N/A"
				pmd.AgentVersion = &s
			}
		} else {
			s := ver.(string)
			pmd.AgentVersion = &s
		}

		latency := mep.api.Peerstore.LatencyEWMA(p)
		if latency != 0 {
			pmd.LatencyEWMA = &latency
		}

		metadata = append(metadata, pmd)
	}

	return metadata, numConns
}

// Config contains all values configured via the standard IPFS config section on plugins.
type Config struct {
	// The interval at which to populate prometheus with statistics about
	// currently connected peers, streams, connections, etc.
	PopulatePrometheusInterval int `json:"PopulatePrometheusInterval"`

	// The number of top N agent versions that are reported to prometheus.
	// AVs are somewhat arbitrarily chosen strings and clutter prometheus.
	AgentVersionCutOff int `json:"AgentVersionCutOff"`

	// Whether to enable the Bitswap discovery probe.
	// This does not work on large monitoring nodes for recent versions of the
	// network, because establishing and holding many Bitswap streams seems to
	// be very resource-hungry.
	EnableBitswapDiscoveryProbe bool `json:"EnableBitswapDiscoveryProbe"`

	// The address of the AMQP server to send real-time data to.
	// If this is empty, the real-time tracer will not be set up.
	AMQPServerAddress string `json:"AMQPServerAddress"`

	// The name to tag real-time data with.
	// If unset, the hostname is used.
	MonitorName *string `json:"MonitorName,omitempty"`

	// Configuration of the HTTP server.
	HTTPServerConfig HTTPServerConfig `json:"HTTPServerConfig"`
}

// Name returns the name of this plugin.
// This is part of the `plugin.Plugin` interface.
func (*MetricExporterPlugin) Name() string {
	return "metric-export-plugin"
}

// Version returns the version of this plugin.
// This is part of the `plugin.Plugin` interface.
func (*MetricExporterPlugin) Version() string {
	return "0.4.0"
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

	if pConf.MonitorName == nil || len(*pConf.MonitorName) == 0 {
		log.Debug("missing MonitorName, trying to get hostname instead...")
		hostName, err := os.Hostname()
		if err != nil {
			return errors.Wrap(err, "unable to determine hostname to use for MonitorName config value")
		}
		log.Warnf("missing MonitorName, using hostname %q instead", hostName)
		log.Warnf("missing MonitorName, using hostname %q instead", hostName)
		pConf.MonitorName = &hostName
	}

	if len(pConf.AMQPServerAddress) == 0 {
		fmt.Println("AMQPServerAddress is empty, will not start Bitswap tracer. Maybe you want to configure this?")
	}

	if !pConf.EnableBitswapDiscoveryProbe {
		fmt.Println("EnableBitswapDiscoveryProbe is false, Bitswap probing functionality will not be available.")
	}

	mep.conf = pConf

	return nil
}

type bitswapOptionsOut struct {
	fx.Out

	BitswapOpts []bs.Option `group:"bitswap-options,flatten"`
}

// tracerSetup provides options for FX to configure Bitswap with a tracer.
// cfg must be a valid Configuration, with MonitorName != nil.
func tracerSetup() interface{} {
	return func(lc fx.Lifecycle, h host.Host, cfg Config) bitswapOptionsOut {
		if len(cfg.AMQPServerAddress) == 0 {
			log.Error("missing AMQPServerAddress, will not add a tracer to Bitswap and will not produce real-time data")
			return bitswapOptionsOut{}
		}

		// Set up tracer, which will attempt to connect to the AMQP server.
		tracer, err := NewTracer(h.Network(), *cfg.MonitorName, cfg.AMQPServerAddress)
		if err != nil {
			panic(err)
		}

		// Subscribe to network events.
		h.Network().Notify(tracer)

		// Add Bitswap option to use our tracer.
		opts := []bs.Option{
			bs.WithTracer(tracer),
		}

		// Add lifecycle stuff to shut down cleanly.
		// TODO I don't think this works.
		lc.Append(fx.Hook{
			OnStop: func(_ context.Context) error {
				tracer.Shutdown()
				return nil
			},
		})

		return bitswapOptionsOut{BitswapOpts: opts}
	}
}

// Options implements FxPlugin.
// This is run _after_ Init and _before_ start, which means we have a validated
// config at this point.
func (mep *MetricExporterPlugin) Options(info core.FXNodeInfo) ([]fx.Option, error) {
	opts := info.FXOptions

	fmt.Println("Metric Export Plugin injecting options...")

	opts = append(opts, fx.Provide(tracerSetup()), fx.Supply(mep.conf))

	return opts, nil
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
	prometheus.MustRegister(probeBitswapSenderCount)
	prometheus.MustRegister(probePeerCount)
	prometheus.MustRegister(probeConnectionCount)
	prometheus.MustRegister(wiretapSentBytes)
	prometheus.MustRegister(wiretapProcessedEvents)

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
	bsnetImpl, ok := (getUnexportedField(reflect.ValueOf(bitswapEngine).Elem().FieldByName("net"))).(bsnet.BitSwapNetwork)
	if !ok {
		return errors.New("could not get Bitswap network implementation")
	}

	if mep.conf.EnableBitswapDiscoveryProbe {
		// Create a bitswap bitswapDiscoveryProbe & subscribe to notifications in Bitswap &
		// network.Network.
		mep.bitswapDiscoveryProbe = NewProbe(ipfsInstance.PeerHost.Network(), bsnetImpl)

		// Subscribe to network events.
		ipfsInstance.PeerHost.Network().Notify(mep.bitswapDiscoveryProbe)
	}

	// Periodically populate Prometheus with metrics about peer counts.
	if mep.conf.PopulatePrometheusInterval != 0 {
		mep.wg.Add(1)
		go mep.populatePrometheus(time.Duration(mep.conf.PopulatePrometheusInterval) * time.Second)
	}

	// Start HTTP server.
	httpServer, err := newHTTPServer(mep, mep.conf.HTTPServerConfig)
	if err != nil {
		return errors.Wrap(err, "unable to start HTTP server")
	}
	mep.httpServer = httpServer

	mep.httpServer.StartServer()

	for _, l := range httpServer.listeners {
		fmt.Printf("Metric Export HTTP server listening on %s\n", l.Addr())
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
	mep.closingLock.Lock()
	select {
	case <-mep.closing:
		// Already closed/closing, no-op.
		return nil
	default:
		close(mep.closing)
	}
	mep.closingLock.Unlock()

	if mep.bitswapDiscoveryProbe != nil {
		mep.bitswapDiscoveryProbe.Shutdown()
	}
	mep.httpServer.Shutdown()

	// Wait for everything to be done.
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
		protocolCounts := make(map[protocol.ID]int)

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
			supportedProtocolsAmongConnectedPeers.With(prometheus.Labels{"protocol": string(protocolID)}).Set(float64(count))
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
