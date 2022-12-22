#!/bin/bash -e

# Shell script to execute a docker builder stage and copy resulting artifacts to the host filesystem.
# This runs Dockerfile.builder and copies the binaries to out/.

mkdir -p out

docker build -t ipfs-mexport-builder -f Dockerfile.builder .
docker build -t kubo-mexport -f Dockerfile.kubo .

docker create --name extract ipfs-mexport-builder
docker cp extract:/usr/local/bin/ipfs ./out/
docker rm extract

mv out/ipfs/* out
rm -R out/ipfs
