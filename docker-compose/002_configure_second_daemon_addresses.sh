#!/bin/sh -ex

# Bump all ports by one, so the node announces correct addresses.
# This needs to be in line with the external ports configured via docker.
ipfs config --json 'Addresses.Swarm' '[
  "/ip4/0.0.0.0/tcp/4002",
  "/ip4/0.0.0.0/udp/4002/quic",
  "/ip4/0.0.0.0/udp/4002/quic-v1",
  "/ip4/0.0.0.0/udp/4002/quic-v1/webtransport",
  "/ip6/::/tcp/4002",
  "/ip6/::/udp/4002/quic",
  "/ip6/::/udp/4002/quic-v1",
  "/ip6/::/udp/4002/quic-v1/webtransport"
]'
