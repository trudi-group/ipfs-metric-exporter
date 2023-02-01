#!/bin/sh -ex

# This disables mDNS discovery, which is generally useful in datacenters.
ipfs config profile apply server

# Disable the resource and connection managers, we want all the connections.
ipfs config --bool 'Swarm.ResourceMgr.Enabled' false

# Configure the ConnMgr to still manage connections, but without an upper limit.
ipfs config --json 'Swarm.ConnMgr' '{
  "GracePeriod": "0s",
  "HighWater": 100000,
  "LowWater": 0,
  "Type": "basic"
}'

# We don't really want to be a relay for people.
ipfs config --bool 'Swarm.RelayService.Enabled' false

# Configure the plugin.
ipfs config --json 'Plugins.Plugins.metric-export-plugin' '{
  "Config": {
    "AMQPServerAddress": "amqp://rmq:5672/%2f",
    "AgentVersionCutOff": 50,
    "HTTPServerConfig": {
      "ListenAddresses": [
        "0.0.0.0:8432"
      ]
    },
    "PopulatePrometheusInterval": 10,
    "EnableBitswapDiscoveryProbe": false
  }
}'

mkdir -p $IPFS_PATH/plugins
cp /mexport-plugin/*.so $IPFS_PATH/plugins/
