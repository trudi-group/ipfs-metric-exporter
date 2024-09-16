package metricplugin

import (
	"context"
	"math/rand"
	"sync"
	"time"

	bsmsg "github.com/ipfs/boxo/bitswap/message"
	pbmsg "github.com/ipfs/boxo/bitswap/message/pb"
	bsnet "github.com/ipfs/boxo/bitswap/network"
	cid "github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	ma "github.com/multiformats/go-multiaddr"
	"go.uber.org/zap"
)

var _ network.Notifiee = (*BitswapDiscoveryProbe)(nil)

// BitswapDiscoveryProbe exposes functionality to perform Bitswap-only discovery
// for a set of CIDs.
// It implements `network.Notifiee` to receive notifications about peers
// connecting, disconnecting, etc.
// It keeps track of peers the node is connected to and attempts to hold an
// active Bitswap sender to each of them.
//
// Using the `network.Notifiee` events is somewhat wonky: We occasionally
// receive duplicate disconnection events or miss connection events.
// However, it looks like we converge with the true IPFS connectivity after a
// while.
type BitswapDiscoveryProbe struct {
	nodeNetwork network.Network
	bsnetImpl   bsnet.BitSwapNetwork

	// Keeps track of peers we are connected to and bitswap senders.
	connManager connectionManager

	// Lifetime management.
	closing   chan struct{}
	closeLock sync.Mutex
	wg        sync.WaitGroup
}

// Tracks which peers we are connected to, and whether we have a Bitswap sender
// to them.
type connectionManager struct {
	lock           sync.Mutex
	allConnections map[string]struct{}
	peers          map[peer.ID]peerConnection
}

// Tracks connections to a single peer and whether we have a Bitswap sender to
// this peer.
type peerConnection struct {
	bitswapSender bsnet.MessageSender
	connections   map[string]network.Conn
}

// NewProbe creates a new Bitswap Probe and network Notifiee.
// This will spawn goroutines to log stats and connect to peers via Bitswap.
// Use the Shutdown method to shut down cleanly.
func NewProbe(nodeNetwork network.Network, bsnetImpl bsnet.BitSwapNetwork) *BitswapDiscoveryProbe {
	wt := &BitswapDiscoveryProbe{
		nodeNetwork: nodeNetwork,
		bsnetImpl:   bsnetImpl,
		connManager: connectionManager{
			allConnections: make(map[string]struct{}),
			peers:          make(map[peer.ID]peerConnection),
		},
		closing: make(chan struct{}),
	}

	// Start a goroutine to periodically go through all peers and open Bitswap
	// senders if we're missing any.
	wt.wg.Add(1)
	go wt.bitswapConnectionLoop()

	// Start a goroutine to periodically print stats about our perceived
	// connectivity and Bitswap senders.
	// This also populates prometheus.
	wt.wg.Add(1)
	go wt.connectivityStatLoop()

	return wt
}

// WaitGroup-bound function to loop forever and print stats about connectivity
// and Bitswap senders.
// This exits when wt.closing is closed.
func (wt *BitswapDiscoveryProbe) connectivityStatLoop() {
	defer wt.wg.Done()

	// A list of Bitswap protocol IDs.
	// These are the official IPFS implementations of Bitswap.
	// There are more (e.g., ipfs-embed), but we can't speak those.
	bitswapProtocols := map[protocol.ID]struct{}{
		bsnet.ProtocolBitswapNoVers:  {},
		bsnet.ProtocolBitswapOneZero: {},
		bsnet.ProtocolBitswapOneOne:  {},
		bsnet.ProtocolBitswap:        {},
	}

	// Ticker for periodic printing/prometheus populating.
	ticker := time.NewTicker(time.Duration(10) * time.Second)

	for {
		select {
		case <-wt.closing:
			log.Debug("Wiretap connectivity stat loop exiting.")
			return
		case <-ticker.C:
		}

		// Get stats as reported by the IPFS node.
		conns := wt.nodeNetwork.Conns()
		peers := make(map[peer.ID]struct{})
		numBitswapStreams := 0
		for _, conn := range conns {
			peers[conn.RemotePeer()] = struct{}{}

			streams := conn.GetStreams()
			for _, stream := range streams {
				proto := stream.Protocol()
				if _, ok := bitswapProtocols[proto]; ok {
					numBitswapStreams++
				}
			}
		}

		// Get stats as seen by us.
		wt.connManager.lock.Lock()
		numOurPeers := len(wt.connManager.peers)
		numOurBitswapSenders := 0
		for _, peerConn := range wt.connManager.peers {
			if peerConn.bitswapSender != nil {
				numOurBitswapSenders++
			}
		}
		numOurConns := len(wt.connManager.allConnections)
		wt.connManager.lock.Unlock()

		// Log some useful info for debugging.
		log.Infof("IPFS reports %d connections to %d peers, with %d bitswap streams. We track %d connections to %d peers (%d bitswap senders)",
			len(conns), len(peers), numBitswapStreams, numOurConns, numOurPeers, numOurBitswapSenders)

		// Populate prometheus.
		probeBitswapSenderCount.Set(float64(numOurBitswapSenders))
		probePeerCount.Set(float64(numOurPeers))
		probeConnectionCount.Set(float64(numOurConns))
	}
}

// WaitGroup-bound function to periodically create missing Bitswap senders.
// This will go through all peers we believe we are connected to and creates a
// goroutine for each missing Bitswap sender, calling connectBitswap.
// Exits when wt.closing is closed.
func (wt *BitswapDiscoveryProbe) bitswapConnectionLoop() {
	defer wt.wg.Done()

	// Ticker for periodic Bitswap sender creation.
	ticker := time.NewTicker(time.Duration(30) * time.Second)

	for {
		select {
		case <-wt.closing:
			log.Debug("bitswap connection loop exiting")
			return
		case <-ticker.C:
		}

		wt.connManager.lock.Lock()
		for peerID, peerConn := range wt.connManager.peers {
			if peerConn.bitswapSender == nil {
				// We use some random sleep duration before connecting.
				// I hope that makes things nicer.
				wt.wg.Add(1)
				go wt.connectBitswap(peerID, time.Duration(rand.Intn(5000))*time.Millisecond)
			}
		}
		wt.connManager.lock.Unlock()
	}
}

// Shutdown shuts down the probe cleanly.
// This is idempotent, i.e. calling it multiple times does not cause problems.
// This will block until all goroutines have quit (this depends on some
// timeouts for connecting to peers and opening streams, probably).
func (wt *BitswapDiscoveryProbe) Shutdown() {
	wt.closeLock.Lock()
	select {
	case <-wt.closing:
	// Already closing
	default:
		close(wt.closing)
	}
	wt.closeLock.Unlock()

	// close bitswap senders?

	wt.wg.Wait()
}

func (wt *BitswapDiscoveryProbe) broadcastBitswapWant(cids []cid.Cid) []BroadcastWantStatus {
	status := wt.broadcast(cids, true, 0, false)

	results := make([]BroadcastWantStatus, len(status))
	for i := range status {
		results[i] = *status[i].request
	}

	return results
}

func (wt *BitswapDiscoveryProbe) broadcastBitswapCancel(cids []cid.Cid) []BroadcastCancelStatus {
	status := wt.broadcast(cids, false, 0, false)

	results := make([]BroadcastCancelStatus, len(status))
	for i := range status {
		results[i] = *status[i].cancel
	}

	return results
}

func (wt *BitswapDiscoveryProbe) broadcastBitswapWantCancel(cids []cid.Cid, secondsBetween uint) []BroadcastWantCancelStatus {
	status := wt.broadcast(cids, true, secondsBetween, true)

	results := make([]BroadcastWantCancelStatus, len(status))
	for i := range status {
		results[i].WantStatus = BroadcastWantCancelWantStatus{
			BroadcastSendStatus: BroadcastSendStatus{
				TimestampBeforeSend: status[i].request.TimestampBeforeSend,
				SendDurationMillis:  status[i].request.SendDurationMillis,
				Error:               status[i].request.Error,
			},
			RequestTypeSent: status[i].request.RequestTypeSent,
		}
		results[i].CancelStatus = BroadcastSendStatus{
			TimestampBeforeSend: status[i].cancel.TimestampBeforeSend,
			SendDurationMillis:  status[i].cancel.SendDurationMillis,
			Error:               status[i].cancel.Error,
		}
		results[i].Peer = status[i].request.Peer
	}

	return results
}

type broadcastStatus struct {
	request *BroadcastWantStatus
	cancel  *BroadcastCancelStatus
}

func (wt *BitswapDiscoveryProbe) broadcast(cids []cid.Cid, want bool, cancelAfterSeconds uint, cancelAfter bool) []broadcastStatus {
	logger := log.Named("broadcast")

	logger.With("want", want, "cancelAfterSeconds", cancelAfterSeconds, "cancelAfter", cancelAfter).Debugf("starting broadcast")

	before := time.Now()

	// Get a copy of all peer IDs we're tracking
	wt.connManager.lock.Lock()
	peerIDs := make([]peer.ID, 0, len(wt.connManager.peers))
	numSenders := 0
	for id, p := range wt.connManager.peers {
		peerIDs = append(peerIDs, id)
		if p.bitswapSender != nil {
			numSenders++
		}
	}
	wt.connManager.lock.Unlock()
	logger.Debugf("Operating on %d peer IDs with %d Bitswap senders currently", len(peerIDs), numSenders)

	// Number of worker goroutines
	numGoroutines := 8
	logger.Debugf("Will spawn %d senders", numGoroutines)

	// Set up some plumbing
	wg := sync.WaitGroup{}
	collectorWg := sync.WaitGroup{}
	resultsChan := make(chan broadcastStatus)
	results := make([]broadcastStatus, 0, numSenders)

	// Spawn a goroutine to collect results
	collectorWg.Add(1)
	go func() {
		defer collectorWg.Done()
		for res := range resultsChan {
			results = append(results, res)
		}
	}()

	// Spawn goroutines to do the work
	workChan := make(chan peer.ID)
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// We allocate and reuse one message to save strain on the GC.
			msg := bsmsg.New(false)
			for peerID := range workChan {
				wt.connManager.lock.Lock()
				p, ok := wt.connManager.peers[peerID]
				if !ok {
					// No longer connected, skip this peer
					logger.Debugf("Sender: no longer connected to %s, skipping", peerID)
				}
				sender := p.bitswapSender
				wt.connManager.lock.Unlock()
				if sender == nil {
					// No sender, skip this peer
					logger.Debugf("Sender: No sender for %s, skipping", peerID)
					continue
				}

				sendBroadcastSinglePeer(want, sender, cids, msg, logger, peerID, resultsChan, cancelAfterSeconds, cancelAfter, &wg)
			}
		}()
	}

	// Feed work into workers
	for _, id := range peerIDs {
		workChan <- id
	}
	close(workChan)
	logger.Debug("Finished distributing work to senders, now waiting for results")

	// Wait for workers to finish
	wg.Wait()
	// Close results channel
	close(resultsChan)
	// Wait for collector to finish
	collectorWg.Wait()

	logger.Debugf("Got %d results, operated on %d peers (with %d Bitswap senders originally), took %s", len(results), len(peerIDs), numSenders, time.Since(before))

	return results
}

func sendBroadcastSinglePeer(want bool, sender bsnet.MessageSender, cids []cid.Cid, msg bsmsg.BitSwapMessage, logger *zap.SugaredLogger, peerID peer.ID, resultsChan chan broadcastStatus, cancelAfterSeconds uint, cancelAfter bool, wg *sync.WaitGroup) {
	status := broadcastStatus{}

	if want {
		wantStatus := sendBitswapWant(sender, msg, logger, peerID, cids)
		status.request = &wantStatus

		if cancelAfter {
			// We do this in another goroutine to not lower concurrency by sleeping here.
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(time.Duration(cancelAfterSeconds) * time.Second)

				cancelStatus := sendBitswapCancel(sender, bsmsg.New(false), logger, peerID, cids)
				status.cancel = &cancelStatus
				resultsChan <- status
			}()
		} else {
			resultsChan <- status
		}
	} else {
		cancelStatus := sendBitswapCancel(sender, msg, logger, peerID, cids)
		status.cancel = &cancelStatus
		resultsChan <- status
	}
}

func sendBitswapWant(sender bsnet.MessageSender, msg bsmsg.BitSwapMessage, logger *zap.SugaredLogger, peerID peer.ID, cids []cid.Cid) BroadcastWantStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg.Reset(false)

	reqType := pbmsg.Message_Wantlist_Have
	if !sender.SupportsHave() {
		reqType = pbmsg.Message_Wantlist_Block
	}
	for _, c := range cids {
		size := msg.AddEntry(c, 2147483647, reqType, true)
		if size == 0 {
			panic("message already contains an entry for this CID")
		}
	}

	logger.Debugf("Sender: will send message requesting %s to %s", reqType, peerID)

	tsBefore, elapsed, err := sendBitswapMessage(ctx, sender, msg, logger, peerID)

	requestStatus := BroadcastWantStatus{
		BroadcastStatus: BroadcastStatus{
			BroadcastSendStatus: BroadcastSendStatus{
				TimestampBeforeSend: tsBefore,
				SendDurationMillis:  elapsed.Milliseconds(),
				Error:               err,
			},
			Peer: peerID,
		},
	}
	if err == nil {
		requestStatus.RequestTypeSent = &reqType
	}

	return requestStatus
}

func sendBitswapCancel(sender bsnet.MessageSender, msg bsmsg.BitSwapMessage, logger *zap.SugaredLogger, peerID peer.ID, cids []cid.Cid) BroadcastCancelStatus {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msg.Reset(false)

	for _, c := range cids {
		size := msg.Cancel(c)
		if size == 0 {
			panic("message already contains an entry for this CID")
		}
	}

	logger.Debugf("Sender: will send message to CANCEL to %s", peerID)

	tsBefore, elapsed, err := sendBitswapMessage(ctx, sender, msg, logger, peerID)

	return BroadcastCancelStatus{
		BroadcastStatus: BroadcastStatus{
			BroadcastSendStatus: BroadcastSendStatus{
				TimestampBeforeSend: tsBefore,
				SendDurationMillis:  elapsed.Milliseconds(),
				Error:               err,
			},
			Peer: peerID,
		},
	}
}

func sendBitswapMessage(ctx context.Context, sender bsnet.MessageSender, msg bsmsg.BitSwapMessage, logger *zap.SugaredLogger, peerID peer.ID) (time.Time, time.Duration, error) {
	// We take two timestamps: one before sending, and one after send finishes.
	// Apparently the remote already starts processing before we return from SendMsg.
	// That can lead to situations where we receive a response _before_ we timestamp the outgoing
	// message, if we only use the timestamp after SendMsg.
	// This then leads to negative response times, which is weird.
	tsBefore := time.Now()
	err := sender.SendMsg(ctx, msg)
	elapsed := time.Since(tsBefore)

	if err != nil {
		logger.Debugf("Sender: failed to send to %s: %s", peerID, err)
	} else {
		logger.Debugf("Sender: successfully sent message to %s", peerID)
	}

	return tsBefore, elapsed, err
}

// Listen is called when the network implementation starts listening on the given address.
// We do not use this at the moment.
func (*BitswapDiscoveryProbe) Listen(network.Network, ma.Multiaddr) {}

// ListenClose is called when the network implementation stops listening on the given address.
// We do not use this at the moment.
func (*BitswapDiscoveryProbe) ListenClose(network.Network, ma.Multiaddr) {}

// Connected is called when a connection is opened.
func (wt *BitswapDiscoveryProbe) Connected(_ network.Network, conn network.Conn) {
	remotePeer := conn.RemotePeer()
	log.Debugf("Connection event for peer %s, address: %s", remotePeer, conn.RemoteMultiaddr())

	// Track connection/peer connectivity.
	wt.connManager.lock.Lock()
	defer wt.connManager.lock.Unlock()
	if _, ok := wt.connManager.allConnections[conn.ID()]; ok {
		log.Warnf("Duplicate connection ID %s (Duplicate Connection Event)", conn.ID())
		// What now?
	}
	wt.connManager.allConnections[conn.ID()] = struct{}{}

	peerConn, ok := wt.connManager.peers[remotePeer]
	if !ok {
		peerConn = peerConnection{
			bitswapSender: nil,
			connections:   map[string]network.Conn{conn.ID(): conn},
		}
	} else {
		if _, ok := peerConn.connections[conn.ID()]; ok {
			log.Warnf("Duplicate connection ID %s to peer %s", conn.ID(), remotePeer)
			// What now?
		}
		peerConn.connections[conn.ID()] = conn
	}
	wt.connManager.peers[remotePeer] = peerConn

	// Connect via Bitswap
	// Check if we're shutting down
	select {
	case <-wt.closing:
		log.Debugf("Connection event handler: not starting bitswap connector to %s because we're shutting down", conn.ID())
	default:
		// Open Bitswap connection in the background
		wt.wg.Add(1)
		go wt.connectBitswap(conn.RemotePeer(), time.Duration(1)*time.Second)
	}
}

// Creates a Bitswap sender to a given peer after waiting the specified time.
// This is supposed to be run as a goroutine and will decrement wt.wg!
//
// Creating a Bitswap sender instructs the IPFS node to connect to the given
// peer, open a Bitswap stream, and creates a message sender on top of that
// stream.
// We will check if we're still connected to the peer, to avoid re-opening
// connections.
// We also check if any of the connections we have to a peer are non-transient.
// If we're still connected, we instruct Bitswap to create a new sender.
// This is racy: The connection could have been closed in the meantime.
// In that case, Bitswap will reconnect, which can take some time (or fail).
// We re-check our connectivity to the peer after we created the sender.
// If we're not connected anymore (for whatever reason), we destroy the sender.
//
// It can happen that the connection that triggered the connection event which
// then triggered us to connect via Bitswap is not the connection through which
// the Bitswap sender communicates.
// This is generally not a problem: The Bitswap sender, when instructed to send
// a message, will retry and reconnect to its peer, using whatever connection is
// available to that peer.
func (wt *BitswapDiscoveryProbe) connectBitswap(remotePeer peer.ID, waitTime time.Duration) {
	defer wt.wg.Done()

	// Wait a few seconds, maybe this was a short-lived connection.
	time.Sleep(waitTime)

	// Check if
	// 1. We're still connected to the peer, and
	// 2. we already have a bitswap sender for that peer
	// 3. we have any non-transient connections to that peer
	wt.connManager.lock.Lock()
	peerEntry, ok := wt.connManager.peers[remotePeer]
	if !ok {
		log.Debugf("bitswap connector: no longer connected to peer %s, not opening a sender", remotePeer)
		wt.connManager.lock.Unlock()
		return
	}
	if peerEntry.bitswapSender != nil {
		log.Debugf("bitswap connector: already have a sender to peer %s, not opening a sender", remotePeer)
		wt.connManager.lock.Unlock()
		return
	}
	anyNonTransient := false
	for _, conn := range peerEntry.connections {
		if !conn.Stat().Transient {
			anyNonTransient = true
		}
	}
	if !anyNonTransient {
		log.Debugf("bitswap connector: we have only transient connections to peer %s, not opening a sender", remotePeer)
		wt.connManager.lock.Unlock()
		return
	}
	wt.connManager.lock.Unlock()

	// The connection is still alive, and we don't have a sender yet.
	// Try to create a sender and add it to the peer.
	// We don't hold the connManager closingLock the entire time because connecting
	// can take some time, e.g. if the connection is actually dead.

	// Leave this blank to use defaults
	// TODO we could make this configurable.
	opts := bsnet.MessageSenderOpts{}
	ctx := context.Background()

	log.Debugf("bitswap connector: trying to create sender to peer %s", remotePeer)
	sender, err := wt.bsnetImpl.NewMessageSender(ctx, remotePeer, &opts)
	if err != nil {
		log.Debugf("bitswap connector: unable to create sender to peer %s: %s", remotePeer, err)
		// TODO maybe retry? But the sender retries internally... Also, the
		// periodic bitswap connection goroutine should find this and clean up.
		return
	}
	log.Debugf("bitswap connector: created sender to peer %s", remotePeer)

	// Add the sender to the peer.
	// We have to re-check if we are still connected to the peer.
	// It does not matter which connection is used to the peer, as the Bitswap
	// sender internally uses whatever connection is available to the peer.
	wt.connManager.lock.Lock()
	defer wt.connManager.lock.Unlock()
	peerEntry, ok = wt.connManager.peers[remotePeer]
	if !ok {
		// Clean up.
		log.Debugf("bitswap connector: no longer connected to peer %s, closing sender", remotePeer)
		_ = sender.Close()
		return
	}
	if peerEntry.bitswapSender != nil {
		// This should rarely happen, but who knows.
		log.Debugf("bitswap connector: already have a sender to peer %s, closing sender", remotePeer)
		_ = sender.Close()
		return
	}

	log.Debugf("bitswap connector: adding new sender to peer %s", remotePeer)
	peerEntry.bitswapSender = sender
	wt.connManager.peers[remotePeer] = peerEntry
}

// Disconnected is called when a connection is closed.
func (wt *BitswapDiscoveryProbe) Disconnected(_ network.Network, conn network.Conn) {
	remotePeer := conn.RemotePeer()
	log.Debugf("Disconnection event for peer %s, address %s", remotePeer, conn.RemoteMultiaddr())

	wt.connManager.lock.Lock()
	defer wt.connManager.lock.Unlock()
	if _, ok := wt.connManager.allConnections[conn.ID()]; !ok {
		log.Warnf("Missing connection ID %s (Duplicate Disconnection Event or Missing Connection Event)", conn.ID())
		// What now?
	}
	delete(wt.connManager.allConnections, conn.ID())

	peerConn, ok := wt.connManager.peers[remotePeer]
	if !ok {
		log.Warnf("Missing entry for disconnected peer %s", remotePeer)
		// What now?
	} else {
		if _, ok := peerConn.connections[conn.ID()]; !ok {
			log.Warnf("Missing connection entry with ID %s for p %s", conn.ID(), remotePeer)
			// What now?
		}
		delete(peerConn.connections, conn.ID())

		if len(peerConn.connections) == 0 {
			// Clean up Bitswap sender
			if peerConn.bitswapSender != nil {
				_ = peerConn.bitswapSender.Close()
			}

			delete(wt.connManager.peers, remotePeer)
		}
	}
}
