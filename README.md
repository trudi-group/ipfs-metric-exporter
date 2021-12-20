# IPFS Metrics Exporter Plugin

An IPFS plugin to export additional metrics and enable real-time analysis of Bitswap traffic.

## Building

Due to a [bug in the Go compiler](https://github.com/cespare/xxhash/issues/54) it is not possible to build plugins
correctly using Go 1.17.
__You need to use Go 1.16 to build both this plugin and the IPFS binary.__

This is an internal plugin, which needs to be built against the sources that produced the `ipfs` binary this plugin will
plug into.
__The `ipfs` binary and this plugin must be built from/against the same IPFS sources, using the same version of the Go
compiler.__
We build and run against go-ipfs v0.10.0, using Go 1.16 due to aforementioned bug in 1.17.
You can build against either
1. the official, online `go-ipfs` source (and recompile IPFS with Go 1.16) or
2. a local fork, in which case you need to a `replace` directive to the `go.mod` file.

There is a [Makefile](./Makefile) which does a lot of this for you.
It respects the `IPFS_PATH` and `IPFS_VERSION` variables, which are otherwise set to sensible defaults.
Use `make build` to build the plugin and `make install` to copy the compiled plugin to the
[IPFS plugin directory](https://github.com/ipfs/go-ipfs/blob/master/docs/plugins.md).

Manually, building with `go build -buildmode=plugin -o mexport.so` should also work.
This will produce a `mexport.so` library which needs to be placed in the IPFS plugin directory, which is
`$IPFS_PATH/plugins` by default.

## Configuration

This plugin can be configured using the usual IPFS configuration.

```
...
"Plugins": {
    "Plugins": {
      "metric-export-plugin": {
        "Config": {
          "PopulatePrometheusInterval": 10,
          "AgentVersionCutOff": 20,
          "TCPServerConfig": {
            "ListenAddress": "localhost:8181"
          }
        }
      }
    }
  },
...
```

### `PopulatePrometheusInterval`

The plugin periodically goes through the node's peer store, open connections, and open streams to collect various metrics about them.
These metrics are then pushed to Prometheus.
This value controls the interval at which this is done, specified in seconds.
Prometheus itself only scrapes (by default) every 15 seconds, so very small values are probably not useful.
The default is ten seconds.

### `AgentVersionCutOff`

The plugin collects metrics about the agent versions of connected peers.
This value configures a cutoff for how many agent version strings should be reported to prometheus.
The remainder (everything that doesn't fit within the cutoff) is summed and reported as `others` to prometheus.

### `TCPServerConfig`

This configures the TCP server used to export metrics, Bitswap messages, and other stuff in real time.
If this section is missing or `null`, the TCP server will not be started.
See below on how the TCP server works.

The `ListenAddress` field configures the endpoint on which to listen.

## Running

Placing the library in the IPFS plugin path and relaunching the daemon should load and run the plugin.
If you plan on running a large monitoring node as per [this paper](https://arxiv.org/abs/2104.09202) it is recommended
to increase the limit of file descriptors for the IPFS daemon.
The IPFS daemon attempts to do this on its own, but only up to 8192 FDs by default.
This is controllable via the `IPFS_FD_MAX` environment variable.
Setting `IPFS_FD_MAX="100000"` should be sufficient.

## Logs

This plugin uses the usual IPFS logging API.
To see logs produced by this plugin, either set an appropriate global log level (the default is `error`):
```
IPFS_LOGGING="info" ipfs daemon
```

Or, after having started the daemon, configure just this component to emit logs at a certain level or above:
```
ipfs daemon&
ipfs log level metric-export info
ipfs log tail # or something else?
```

## The TCP Server

This plugin comes with a TCP server that exposes a small request-oriented API as well as pushes Bitswap messages and other things to clients.
The protocol consists of 4-byte, big endian, framed, JSON-encoded messages.
Thus, on the wire, each message looks like this:
```
<size of following message in bytes, 4 bytes big endian><JSON-encoded message>
```

A connection starts with a handshake, during which both sides send a version message of this form:
```go
// A version message, exchanged between client and server once, immediately
// after the connection is established.
type versionMessage struct {
	Version int `json:"version"`
}
```
Encoded in the usual manner.
Both sides verify that the version matches.
If there is a mismatch, the connection is closed.
The current version of the API, as described here, is `2`.

### Changelog

Version 1 is the initial version of the format.
It contains the framed messages, requests, and responses.

Version 2 introduces block presences (see the [Bitswap spec](https://github.com/ipfs/go-bitswap/blob/master/docs/how-bitswap-works.md)) to pushed Bitswap messages.

### Messages from Client -> Plugin

Messages going from a client to this plugin have the following format:
```go
// The type of messages coming in via TCP.
type incomingTCPMessage struct {
	// If Request is not nil, this message is a request.
	Request *request `json:"request,omitempty"`
}

type request struct {
	// The ID of the request.
	// This must be unique per client per point in time.
	// Request IDs are used to match responses to requests.
	// They may be reused after a response to an earlier request has been
	// received.
	ID          uint16              `json:"id"`

	// If Subscribe is not nil, this is a Subscribe request.
	// See the PluginAPI interface.
	Subscribe   *subscribeRequest   `json:"subscribe,omitempty"`

	// If Unsubscribe is not nil, this is an Unsubscribe request.
	// See the PluginAPI interface.
	Unsubscribe *unsubscribeRequest `json:"unsubscribe,omitempty"`

	// If Ping is not nil, this is a Ping request.
	// See the PluginAPI interface.
	Ping        *pingRequest        `json:"ping,omitempty"`
}

type subscribeRequest struct{}

type unsubscribeRequest struct{}

type pingRequest struct{}
```

### Messages from Plugin -> Client

Messages originating from this plugin have the following format:
```go
// The type of messages sent out via TCP.
type outgoingTCPMessage struct {
	// If Event is not nil, this message is a pushed event.
	Event *event `json:"event,omitempty"`

	// If Response is not nil, this message is a response to an earlier request.
	Response *response `json:"response,omitempty"`
}

type response struct {
	// The ID of the response.
	// This matches the response to an earlier request.
	ID uint16 `json:"id"`

	// If Subscribe is not nil, this message is the response to a
	// Subscribe request.
	Subscribe *subscribeResponse `json:"subscribe,omitempty"`

	// If Unsubscribe is not nil, this message is the response to a
	// Unsubscribe request.
	Unsubscribe *unsubscribeResponse `json:"unsubscribe,omitempty"`

	// If Ping is not nil, this message is the response to a
	// Ping request.
	Ping *pingResponse `json:"ping,omitempty"`
}

type subscribeResponse struct {
	// Propagates the error in string representation from the Subscribe method.
	Error string `json:"error"`
}

type unsubscribeResponse struct{}

type pingResponse struct{}
```

### Events

A client is, by default, not subscribed to events emitted by this plugin.
Events sent by this plugin are of this format:
```go
// The type sent to via TCP for pushed events.
type event struct {
	// The timestamp at which the event was recorded.
	// This defines an ordering for events.
	Timestamp time.Time `json:"timestamp"`

	// Peer is a base58-encoded string representation of the peer ID.
	Peer string `json:"peer"`

	// BitswapMessage is not nil if this event is a bitswap message.
	BitswapMessage *BitswapMessage `json:"bitswap_message,omitempty"`

	// ConnectionEvent is not nil if this event is a connection event.
	ConnectionEvent *ConnectionEvent `json:"connection_event,omitempty"`
}
```

See also the [sources](metricplugin/tcp.go).

The `BitswapMessage` and `ConnectionEvent` structs are specified in [metricplugin/api.go](metricplugin/api.go).

## License

MIT, see [LICENSE](./LICENSE).