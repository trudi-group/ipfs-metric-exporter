package metricplugin

import (
	"time"

	"github.com/libp2p/go-libp2p/core/protocol"

	bsmsg "github.com/ipfs/boxo/bitswap/message"
	pbmsg "github.com/ipfs/boxo/bitswap/message/pb"
	cid "github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// A BitswapMessage is the type pushed to remote clients for recorded incoming
// Bitswap messages.
type BitswapMessage struct {
	// Wantlist entries sent with this message.
	WantlistEntries []bsmsg.Entry `json:"wantlist_entries"`

	// Whether the wantlist entries are a full new wantlist.
	FullWantList bool `json:"full_wantlist"`

	// Blocks sent with this message.
	Blocks []cid.Cid `json:"blocks"`

	// Block presence indicators sent with this message.
	BlockPresences []BlockPresence `json:"block_presences"`

	// Underlay addresses of the peer we were connected to when the message
	// was received.
	ConnectedAddresses []ma.Multiaddr `json:"connected_addresses"`
}

// A BlockPresence indicates the presence or absence of a block.
type BlockPresence struct {
	// Cid is the referenced CID.
	Cid cid.Cid `json:"cid"`

	// Type indicates the block presence type.
	Type BlockPresenceType `json:"block_presence_type"`
}

// BlockPresenceType is an enum for presence or absence notifications.
type BlockPresenceType int

// Block presence constants.
const (
	// Have indicates that the peer has the block.
	Have BlockPresenceType = 0
	// DontHave indicates that the peer does not have the block.
	DontHave BlockPresenceType = 1
)

// ConnectionEventType specifies the type of connection event.
type ConnectionEventType int

const (
	// Connected specifies that a connection was opened.
	Connected ConnectionEventType = 0
	// Disconnected specifies that a connection was closed.
	Disconnected ConnectionEventType = 1
)

// A ConnectionEvent is the type pushed to remote clients for recorded
// connection events.
type ConnectionEvent struct {
	// The multiaddress of the remote peer.
	Remote ma.Multiaddr `json:"remote"`

	// The type of this event.
	ConnectionEventType ConnectionEventType `json:"connection_event_type"`
}

// The RPCAPI is the interface for RPC-like method calls.
// These are served via HTTP, reliably.
type RPCAPI interface {
	// Ping is a no-op.
	Ping()

	// BroadcastBitswapWant broadcasts WANT_(HAVE|BLOCK) requests for the given
	// CIDs to all connected peers that support Bitswap.
	// Which request type to send is chosen by the capabilities of the remote
	// peer.
	// This is sent as one message, which is either sent completely or fails.
	// An error is returned if Bitswap discovery is unavailable.
	BroadcastBitswapWant(cids []cid.Cid) ([]BroadcastWantStatus, error)

	// BroadcastBitswapCancel broadcasts CANCEL entries for the given CIDs to
	// all connected peers that support Bitswap.
	// This is sent as one message, which is either sent completely or fails.
	// An error is returned if Bitswap discovery is unavailable.
	BroadcastBitswapCancel(cids []cid.Cid) ([]BroadcastCancelStatus, error)

	// BroadcastBitswapWantCancel broadcasts WANT_(HAVE|BLOCK) requests for the
	// given CIDs, followed by CANCEL entries after a given time to all
	// connected peers that support Bitswap.
	// An error is returned if Bitswap discovery is unavailable.
	BroadcastBitswapWantCancel(cids []cid.Cid, secondsBetween uint) ([]BroadcastWantCancelStatus, error)

	// SamplePeerMetadata returns information about seen/connected Peers.
	// It returns information about all peers of the peerstore (including past
	// peers not currently connected) as well as the number of currently
	// established connections.
	// The onlyConnected parameter specifies whether data should be filtered
	// for currently connected peers. This can be useful, as the size of the
	// peerstore, and thus the metadata returned by this function, grows without
	// bound.
	SamplePeerMetadata(onlyConnected bool) ([]PeerMetadata, int)
}

// PluginAPI describes the functionality provided by this monitor to remote
// clients.
type PluginAPI interface {
	RPCAPI
}

// BroadcastSendStatus contains basic information about a send operation to
// a single peer as part of a Bitswap broadcast.
type BroadcastSendStatus struct {
	TimestampBeforeSend time.Time `json:"timestamp_before_send"`
	SendDurationMillis  int64     `json:"send_duration_millis"`
	Error               error     `json:"error,omitempty"`
}

// BroadcastStatus contains additional basic information about a send operation
// to a single peer as part of a Bitswap broadcast.
type BroadcastStatus struct {
	BroadcastSendStatus
	Peer peer.ID `json:"peer"`
	// Underlay addresses of the peer we were connected to when the message
	// was sent, or empty if there was an error.
	ConnectedAddresses []ma.Multiaddr `json:"connected_addresses,omitempty"`
}

// BroadcastWantStatus describes the status of a send operation to a single
// peer as part of a Bitswap WANT broadcast.
type BroadcastWantStatus struct {
	BroadcastStatus
	RequestTypeSent *pbmsg.Message_Wantlist_WantType `json:"request_type_sent,omitempty"`
}

// BroadcastCancelStatus describes the status of a send operation to a single
// peer as part of a Bitswap CANCEL broadcast.
type BroadcastCancelStatus struct {
	BroadcastStatus
}

// BroadcastWantCancelWantStatus contains information about the send-WANT
// operation to a single peer as part of a Bitswap WANT+CANCEL broadcast.
type BroadcastWantCancelWantStatus struct {
	BroadcastSendStatus
	RequestTypeSent *pbmsg.Message_Wantlist_WantType `json:"request_type_sent,omitempty"`
}

// BroadcastWantCancelStatus describes the status of a send operation to a
// single peer as part of a Bitswap WANT+CANCEL broadcast.
type BroadcastWantCancelStatus struct {
	Peer               peer.ID        `json:"peer"`
	ConnectedAddresses []ma.Multiaddr `json:"connected_addresses,omitempty"`

	WantStatus   BroadcastWantCancelWantStatus `json:"want_status"`
	CancelStatus BroadcastSendStatus           `json:"cancel_status"`
}

// PeerMetadata holds metadata about a peer.
// This applies to both currently-connected as well as past peers.
// For peers that are not currently connected, the  ConnectedMultiaddrs field
// will be nil.
// The AgentVersion and LatencyEWMA are optional. They are usually present for
// connected peers. For no-longer-connected peers, these hold the last known
// value.
type PeerMetadata struct {
	// Metadata about every peer.

	// The ID of the peer.
	ID peer.ID `json:"peer_id"`
	// The connectedness, i.e., current connection status.
	Connectedness network.Connectedness `json:"connectedness"`
	// A list of known valid multiaddresses for this peer.
	// If the peer is not currently connected, this information might be
	// outdated.
	Multiaddrs []ma.Multiaddr `json:"multiaddresses"`
	// A list of known supported protocols for this peer.
	// If the peer is not currently connected, this information might be
	// outdated.
	Protocols []protocol.ID `json:"protocols"`

	// Metadata about seen or connected peers, optional.

	// Agent version of the peer.
	// If we are no longer connected, this reports the last-seen agent version.
	// If the agent version is not (yet) known, this is "N/A".
	// If this is null, some other error occurred (which hopefully never
	// happens).
	AgentVersion *string `json:"agent_version"`
	// The EWMA of latencies to the peer.
	// If we are no longer connected, this reports the last-known average.
	// If this is null, we don't have latency information for the peer yet.
	LatencyEWMA *time.Duration `json:"latency_ewma_ns"`

	// Metadata about connected peers, optional.

	// A list of multiaddresses to which we currently hold a connection.
	ConnectedMultiaddrs []ma.Multiaddr `json:"connected_multiaddresses"`
}
