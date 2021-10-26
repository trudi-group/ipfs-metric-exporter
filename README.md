# ipfs-plugin-metric-exporter. A plugin for IPFS which exposes the internal API and reports various monitor metrics..

## Configuration

You need the following snippet in your ipfs-config file:
`
"Plugins": {
    "Plugins": {
      "metric-export-plugin": {
        "Config": {
          "pollInterval": "30s"
        }
      }
    }
  },
`

To build the plugin you have to set ```export IPFS_VERSION=<absolute path of IPFS source>``` you want to bind agains, then ```make build``` and ```make install```.
Alternatively, simply build and move the resulting ```mexport.so``` to your IPFS plugin path, per default ```~/.ipfs/plugins``` (create the plugins directoy if it doesn't exist).
