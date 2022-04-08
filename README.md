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
We build and run against go-ipfs v0.12.0, using Go 1.16 due to aforementioned bug in 1.17.
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

### Docker

You can build this project together with a matching go-ipfs executable within Docker.
This is nice, because you get reproducible, matching binaries, compiled with Go 1.16 on Debian bullseye.
Building on bullseye gives us a libc version which is a bit older.
This gives us compatibility with slightly older systems (e.g. Ubuntu LTS releases), at no loss of functionality.

The [builder Dockerfile](./Dockerfile.builder) implements a builder stage.
The resulting binaries are placed in `/usr/local/bin/ipfs/` inside the image.

The [build-in-docker.sh](./build-in-docker.sh) script executes the builder and copies the produced binaries to the `out/` directory of the project.

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
            "ListenAddresses": [
                "localhost:8181"
            ]
          },
          "HTTPServerConfig": {
            "ListenAddresses": [
                "localhost:8432"
            ]
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

This configures the TCP server used to export a pubsub mechanism for Bitswap monitoring in real time.
If this section is missing or `null`, the TCP server will not be started.
Bitswap monitoring is performed regardless.
See below on how the TCP server works.

The `ListenAddresses` field configures the endpoints on which to listen.

### `HTTPServerConfig`

This configures the HTTP server used to expose an RPC API.
The API is located at `/metric_plugin/v1` and returns JSON-encoded messages.
See below for a list of methods.

The `ListenAddresses` field configures the endpoints on which to listen.

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

## The TCP Server for Real-Time Bitswap Monitoring

This plugin comes with a TCP server that pushes Bitswap monitoring messages to clients.
The protocol consists of 4-byte, big endian, framed, gzipped, JSON-encoded messages.
Thus, on the wire, each message looks like this:
```
<size of following message in bytes, 4 bytes big endian><gzipped JSON-encoded message>
```
Each message is gzipped individually, i.e., the state of the encoder is reset for each message.
There is a [client implementation in Rust](https://github.com/mrd0ll4r/ipfs-tools/tree/master/bitswap-monitoring-client-lib) which works with this.

A connection starts with an **uncompressed** handshake, during which both sides send a version message of this form:
```go
// A version message, exchanged between client and server once, immediately
// after the connection is established.
type versionMessage struct {
	Version int `json:"version"`
}
```
This is JSON-encoded and framed.
Both sides verify that the version matches.
If there is a mismatch, the connection is closed.
The current version of the API, as described here, is `3`.

After the handshake succeeds, clients are automatically subscribed to all Bitswap monitoring messages.
There is a backpressure mechanism: Slow clients will not receive all messages.

### Changelog

Version 1 is the initial version of the format.
It contains the framed messages, requests, and responses.

Version 2 introduces block presences (see the [Bitswap spec](https://github.com/ipfs/go-bitswap/blob/master/docs/how-bitswap-works.md)) to pushed Bitswap messages.

Version 3 introduces gzipping of individual messages and removes all API functionality.
Clients are now automatically subscribed.

### Messages from Plugin -> Client

Messages originating from this plugin have the following format:
```go
// The type of messages sent out via TCP.
type outgoingTCPMessage struct {
	// If Event is not nil, this message is a pushed event.
	Event *event `json:"event,omitempty"`
}
```

A client is, by default, subscribed to events emitted by this plugin.
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

See also the sources: [wire protocol](metricplugin/tcp_client.go) and [server](metricplugin/tcp_server.go).

The `BitswapMessage` and `ConnectionEvent` structs are specified in [metricplugin/api.go](metricplugin/api.go).

## The HTTP Server

The HTTP server exposes an RPC-like API via HTTP.
Successful responses are always JSON-encoded and returned with HTTP code 200.
Unsuccessful requests are indicated by HTTP status codes other than 2xx and may return an error message.

The response format looks like this:
```go
// A JSONResponse is the format for every response returned by the HTTP server.
type JSONResponse struct {
	Status int         `json:"status"`
	Result interface{} `json:"result,omitempty"`
	Err    *string     `json:"error,omitempty"`
}
```

### Methods

Methods are identified by their HTTP path, which always begins with `/metric_plugin/v1`.
The following methods are implemented:

#### `GET /ping`

This is a no-op which returns an empty struct.

### `GET /monitoring_addresses`

Returns a list of TCP endpoints on which the plugin is listening for Bitswap monitoring subscriptions.
If the TCP server is not enabled, this returns an empty list.
Returns a struct of this format:
```go
type monitoringAddressesResponse struct {
	Addresses []string `json:"addresses"`
}
```

## License

MIT, see [LICENSE](./LICENSE).
