# Force Go Modules
GO111MODULE = on

GOCC ?= go
GOFLAGS ?=

# If set, override the install location for plugins
IPFS_PATH ?= $(HOME)/.ipfs

# If set, override the IPFS version to build against. This _modifies_ the local
# go.mod/go.sum files and permanently sets this version.
IPFS_VERSION ?= $(lastword $(shell $(GOCC) list -m github.com/ipfs/kubo))

# make reproducible
ifneq ($(findstring /,$(IPFS_VERSION)),)
# Locally built kubo
GOFLAGS += -asmflags=all=-trimpath="$(GOPATH)" -gcflags=all=-trimpath="$(GOPATH)"
else
# Remote version of kubo (e.g. via `go get -trimpath` or official distribution)
GOFLAGS += -trimpath
endif

.PHONY: install build

go.mod: FORCE
	./set-target.sh $(IPFS_VERSION)

FORCE:

mexport.so: main.go go.mod
	$(GOCC) build $(GOFLAGS) -buildmode=plugin -o "$@" "$<"
	chmod +x "$@"

gen-out:
	docker build -t kubo-mexport .
	docker create --name extract kubo-mexport
	mkdir -p out
	docker cp extract:/usr/local/bin/ipfs ./out/
	docker cp extract:/mexport-plugin/mexport-v0.17.0-docker.so ./out/
	docker rm extract

build: mexport.so
	@echo "Built against" $(IPFS_VERSION)

install: build
	mkdir -p "$(IPFS_PATH)/plugins/"
	cp -f mexport.so "$(IPFS_PATH)/plugins/mexport.so"
