# IPFS Metrics Exporter Plugin

An IPFS plugin to export additional metrics and enable real-time analysis of Bitswap traffic.

## Building

__You must build both the plugin and the host go-ipfs (kubo) from the same sources, using the same compiler.__
The recommended way of building this plugin with a matching kubo version is within a Docker build environment.
Alternatively, see below for background information on the process and manual build instructions.

### Docker

You can build this project together with a matching kubo executable within Docker.
This is nice, because you get reproducible, matching binaries, compiled with Go 1.19 on Debian bullseye.
Building on bullseye gives us a libc version which is a bit older.
This gives us compatibility with slightly older systems (e.g. Ubuntu LTS releases), at no loss of functionality.

The [Dockerfile](./Dockerfile) implements kubo with the bundled plugin.
Running `make gen-out` will build the image and extract the binaries for kubo and the plugin to `./out`.

You can run the binary and matching plugin locally, or via the modified kubo container.
See [the configuration for our monitoring setup](./docker-compose/001_configure_ipfs.sh) for configuration and installation instructions.

### Manually

This is an internal plugin, which needs to be built against the sources that produced the `ipfs` binary this plugin will
plug into.
__The `ipfs` binary and this plugin must be built from/against the same IPFS sources, using the same version of the Go
compiler.__

There is a [Makefile](./Makefile) which does a lot of this for you.
It respects the `IPFS_PATH` and `IPFS_VERSION` variables, which are otherwise set to sensible defaults.
Use `make build` to build the plugin and `make install` to copy the compiled plugin to the
[IPFS plugin directory](https://github.com/ipfs/kubo/blob/master/docs/plugins.md).

Manually, building with `go build -buildmode=plugin -o mexport.so` should also work.
This will produce a `mexport.so` library which needs to be placed in the IPFS plugin directory, which is
`$IPFS_PATH/plugins` by default.

## Updating

Updating for a new version of kubo is usually simple:

```
# Update dependencies
go get -v github.com/ipfs/kubo@<new version>

# Do some housekeeping, I guess?
go mod tidy
go mod download
go mod verify
```

Then update the version tags in the `Dockerfile`.

If required, update the configuration scripts in `docker-compose/`.

### Check

To see if things work, first compile:
```
make gen-out
```
then run the monitoring setup locally:
```
cd docker-compose
docker compose down --volumes
docker compose up
```
and see if that works.

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
          "EnableBitswapDiscoveryProbe": false,
          "AMQPServerAddress": "amqp://localhost:5672",
          "MonitorName": "<some name, defaults to hostname>"
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

### `EnableBitswapDiscoveryProbe`

This controls whether active Bitswap discovery probing functionality should be made available.
The way this works is through maintaining an active Bitswap stream to every connected peer.
Discovery requests (i.e. `WANT_HAVE` or `WANT_BLOCK`, depending on whether the peer supports it) can then be broadcast to all connected peers.
This does not use the usual Bitswap discovery code path, which allows us to _only_ search Bitswap, and not the DHT.

For recent versions of the network and large monitoring nodes, this is too resource-intensive and should probably not be enabled.

### `AMQPServerAddress`

This configures the AMQP server to connect to in order to publish Bitswap messages and connection events.
If this is left empty, no real-time processing of Bitswap messages will take place.

### `MonitorName`

Any events published to AMQP are tagged with the name of the origin monitor.
This configures that name.
If left empty, defaults to the hostname.

### `HTTPServerConfig`

This configures the HTTP server used to expose an RPC API.
The API is located at `/metric_plugin/v1` and returns JSON-encoded messages.
See below for a list of methods.

The `ListenAddresses` field configures the endpoints on which to listen.

## Running

In order to run with the plugin, you need to
1. Copy the compiled plugin to the IPFS plugin directory (which may need to be created).
2. Edit the IPFS configuration to configure the plugin.
3. Launch the __matching kubo__ in daemon mode.

If you plan on running a large monitoring node as per [this paper](https://arxiv.org/abs/2104.09202) it is recommended
to increase the limit of file descriptors for the IPFS daemon.
The IPFS daemon attempts to do this on its own, but only up to 8192 FDs by default.
This is controllable via the `IPFS_FD_MAX` environment variable.
Setting `IPFS_FD_MAX="100000"` should be sufficient.

## Logs

This plugin uses the usual IPFS logging API.
To see logs produced by this plugin, either set an appropriate global log level (the default is `error`):
```
IPFS_LOGGING="info" ipfs daemon | grep -a "metric-export"
```

Or, after having started the daemon, configure just this component to emit logs at a certain level or above:
```
ipfs daemon&
ipfs log level metric-export info
ipfs log tail # or something else?
```

## Prometheus Metrics

The plugin adds additional metrics to the prometheus endpoint provided by the daemon.

### Information About the Node

These are always present.
They track information about connectivity of the node, such as open streams or agent versions of connected peers.

#### `plugin_metric_export_peers_supported_protocols`

Sum of supported protocols over all currently connected peers, as reported by the kubo node.
Labels: `protocol`

#### `plugin_metric_export_peers_by_agent_version`

Number of currently connected peers, distinguished by their agent_version.
Labels: `agent_version`

#### `plugin_metric_export_open_streams`

Number of currently open streams, by protocol and direction, as reported by the kubo node.
Labels: `protocol`, `direction`

### Information About the Bitswap Discovery Probe

These are only present if the Bitswap discovery probe is enabled, which is probably not the case on large monitoring nodes.
The discovery probe maintains long-lived Bitswap senders to all connected peers.
These metrics track the number of peers and connections tracked through the discovery probe, which maintains its state using connection events reported by the node, as well as the number of available Bitswap senders to use for probing.
This is generally a bit out-of-sync with the real connectivity of the node.

#### `plugin_metric_export_bitswap_senders`

Number of available Bitswap senders to use by the discovery probe to send Bitswap messages.

#### `plugin_metric_export_wiretap_peers`

Number of connected peers tracked via the discovery probe, based on connection events reported by the kubo node.

#### `plugin_metric_export_wiretap_connections`

Number of connections tracked via the discovery probe, based on connection events reported by the kubo node.


### Metrics About the Bitswap Wiretap

These are only populated if the real-time Bitswap tracer is set up.
They mostly track the performance of the tracer and pub/sub setup to RabbitMQ.

#### `plugin_metric_export_wiretap_sent_bytes`

Number of bytes processed by the Bitswap tracer to RabbitMQ, by pre/post compression.
Labels: `compressed`

#### `plugin_metric_export_wiretap_processed_events`

Number of events processed by the Bitswap tracer, by whether they were dropped due to backpressure/slow connection to RabbitMQ.
Labels: `dropped`

## Real-Time Monitoring via RabbitMQ

This plugin pushes all messages it receives via Bitswap, as well as connection events, to a RabbitMQ server.
The topic exchange `ipfs.passive_monitoring` is used for this.
Any publishing made to RabbitMQ has a gzipped, JSON-encoded array as its payload.
Each publishing is gzipped individually, i.e., the state of the encoder is reset for each array of events.

The exchange topics are of the form `monitor.<monitor name>.(bitswap_message|conn_events)`.

There is a [client implementation in Rust](https://github.com/trudi-group/ipfs-tools/tree/master/ipfs-monitoring-plugin-client) which works with this.

Events sent by this plugin are of this format:
```go
// The type sent to via TCP for pushed events.
type Event struct {
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

The `BitswapMessage` and `ConnectionEvent` structs are specified in [metricplugin/api.go](metricplugin/api.go).
The different topics of the exchange contain events of only that type.

Internally, some batching is done to reduce the number of messages sent via AMQP.
This increases latency, but is probably necessary for typical performance.

Additionally, if the AMQP connection is too slow, events may be dropped.
The prometheus counter `plugin_metric_export_wiretap_processed_events` keeps track of this.

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

### `POST /broadcast_want` and `POST /broadcast_cancel`

These methods initiate a WANT or CANCEL broadcast to be sent via Bitswap, respectively.
They each expect a list of CIDs, JSON-encoded in a struct of this form:
```go
type broadcastBitswapRequest struct {
	Cids []cid.Cid `json:"cids"`
}
```

They return structs of this form, respectively:
```go
type broadcastBitswapWantResponse struct {
	Peers []broadcastBitswapWantResponseEntry `json:"peers"`
}

type broadcastBitswapSendResponseEntry struct {
	TimestampBeforeSend time.Time `json:"timestamp_before_send"`
	SendDurationMillis  int64     `json:"send_duration_millis"`
	Error               *string   `json:"error,omitempty"`
}

type broadcastBitswapWantResponseEntry struct {
	broadcastBitswapSendResponseEntry
	Peer            peer.ID                          `json:"peer"`
	RequestTypeSent *pbmsg.Message_Wantlist_WantType `json:"request_type_sent,omitempty"`
}

type broadcastBitswapCancelResponse struct {
	Peers []broadcastBitswapCancelResponseEntry `json:"peers"`
}

type broadcastBitswapCancelResponseEntry struct {
	broadcastBitswapSendResponseEntry
	Peer peer.ID `json:"peer"`
}
```

### `POST /broadcast_want_cancel`

This method performs a Bitswap WANT broadcast followed by a CANCEL broadcast, to the same set of peers, after a given number of seconds.
This is useful because each broadcast individually takes a while, which makes it difficult to enforce the correct timing between WANT and CANCEL broadcasts from the perspective of an API client.

Request:
```go
type broadcastBitswapWantCancelRequest struct {
	Cids                []cid.Cid `json:"cids"`
	SecondsBeforeCancel uint32    `json:"seconds_before_cancel"`
}
```

Respose:
```go
type broadcastBitswapWantCancelResponse struct {
	Peers []broadcastBitswapWantCancelResponseEntry `json:"peers"`
}

type broadcastBitswapWantCancelWantEntry struct {
	broadcastBitswapSendResponseEntry
	RequestTypeSent *pbmsg.Message_Wantlist_WantType `json:"request_type_sent,omitempty"`
}

type broadcastBitswapWantCancelResponseEntry struct {
	Peer         peer.ID                             `json:"peer"`
	WantStatus   broadcastBitswapWantCancelWantEntry `json:"want_status"`
	CancelStatus broadcastBitswapSendResponseEntry   `json:"cancel_status"`
}
```

## License

MIT, see [LICENSE](./LICENSE).
