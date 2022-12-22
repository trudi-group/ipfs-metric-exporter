# Modifies the ipfs/kubo image to contain our plugin and a matching kubo version.

FROM ipfs/kubo:v0.17.0

COPY --from=ipfs-mexport-builder /usr/local/bin/ipfs/ipfs-v0.17.0-docker /usr/local/bin/ipfs
COPY --from=ipfs-mexport-builder /usr/local/bin/ipfs/mexport-v0.17.0-docker.so /mexport-plugin/
