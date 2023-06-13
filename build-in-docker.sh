#!/bin/bash -e

mkdir -p out

docker build -t trudi-group/kubo-mexport .

docker create --name extract trudi-group/kubo-mexport
docker cp extract:/usr/local/bin/ipfs ./out/
docker cp extract:/mexport-plugin ./out/
docker rm extract

mv ./out/mexport-plugin/*.so ./out/
rm -R ./out/mexport-plugin
