# Docker environment to build matching kubo and plugin binaries.
# This will compile kubo v0.21.0 and the current sources of our plugin to match that.
# Sources are placed in /usr/src/ipfs/.
# Artifacts are put in /usr/local/bin/ipfs/.

# We need go v1.18+ because of a weird compiler bug in 1.17.
# Also, we want bullseye as the build environment in order to use a somewhat old libc.
# This improves compatibility with older host systems at no loss of functionality.
FROM golang:1.22-bullseye AS builder

# First, get and compile kubo v0.21.0.
# We need matching kubo and plugin executables, so it makes sense to build them together.
# We build kubo first, because, since we're building off a defined tag, the sources don't change and this can be
# cached.
WORKDIR /usr/src/ipfs/kubo
RUN git clone https://github.com/ipfs/kubo.git . && git checkout v0.21.0
RUN go build -v -o /usr/local/bin/ipfs/ipfs-v0.21.0-docker ./cmd/ipfs

# Compile metric export plugin
WORKDIR /usr/src/ipfs/metric-export-plugin
# Get and compile dependencies first, since we can cache those.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Then copy sources and compile project
COPY . .
# We need to add a replace directive to the go.mod file
RUN go mod edit -replace=github.com/ipfs/kubo=../kubo
RUN go build -v -buildmode=plugin -o /usr/local/bin/ipfs/mexport-v0.21.0-docker.so main.go

# Go plugins need to be executable.
RUN chmod +x /usr/local/bin/ipfs/mexport-v0.21.0-docker.so

# Artifacts should now be in /usr/local/bin/ipfs/

# Modify the ipfs/kubo image to contain our plugin and a matching kubo version.
FROM ipfs/kubo:v0.21.0

# Copy artifacts from builder.
COPY --from=builder /usr/local/bin/ipfs/ipfs-v0.21.0-docker /usr/local/bin/ipfs
COPY --from=builder /usr/local/bin/ipfs/mexport-v0.21.0-docker.so /mexport-plugin/
