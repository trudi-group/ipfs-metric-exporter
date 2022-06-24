# Docker environment to build matching go-ipfs and plugin binaries.
# This will compile go-ipfs v0.13.0 and the current sources of our plugin to match that.
# Sources are placed in /usr/src/ipfs/.
# Artifacts are put in /usr/local/bin/ipfs/.

# We need go v1.18 because of a weird compiler bug in 1.17.
# Also, we want bullseye as the build environment in order to use a somewhat old libc.
# This improves compatibility with older host systems at no loss of functionality.
FROM golang:1.18-bullseye AS builder

# First, get and compile go-ipfs v0.13.0.
# We need matching go-ipfs and plugin executables, so it makes sense to build them together.
# We build go-ipfs first, because, since we're building off a defined tag, the sources don't change and this can be
# cached.
WORKDIR /usr/src/ipfs/go-ipfs
RUN git clone https://github.com/ipfs/go-ipfs.git . && git checkout v0.13.0
RUN go build -v -o /usr/local/bin/ipfs/ipfs-v0.13.0-docker ./cmd/ipfs

# Compile metric export plugin
WORKDIR /usr/src/ipfs/metric-export-plugin
# Get and compile dependencies first, since we can cache those.
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Then copy sources and compile project
COPY . .
# We need to add a replace directive to the go.mod file
RUN go mod edit -replace=github.com/ipfs/go-ipfs=../go-ipfs
RUN go build -v -buildmode=plugin -o /usr/local/bin/ipfs/mexport-v0.13.0-docker.so main.go

# Go plugins need to be executable.
RUN chmod +x /usr/local/bin/ipfs/mexport-v0.13.0-docker.so

# Artifacts should now be in /usr/local/bin/ipfs/
