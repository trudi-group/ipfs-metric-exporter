# IPFS Bitswap Monitoring Infrastructure

The infrastructure powering [https://grafana.monitoring.ipfs.trudi.group]().

## Building

This uses two images you'll need to build locally:

- `bitswap-monitoring-client:latest` is the monitoring client.
    This is most easily built using [the build script in the ipfs-tools repository](https://github.com/trudi-group/ipfs-tools/blob/master/build-docker-images.sh).
- `monitoring-size-estimator:latest` is the size estimator.
    This, too, is most easily built using above build script.

## Running

### Networking

We run a VPN for our infrastructure and reverse proxy the grafana on a different server.
This is realized using the `LOCAL_IP` environment variable.
You may need to change some of the addresses in the docker compose file according to your own setup.

### Environment

- `GEOIP_ACCOUNT_ID`  The account ID for MaxMind's GeoLite database. You need a free account.
- `GEOIP_LICENSE_KEY` The license key for MaxMind's GeoLite database.
- `GRAFANA_ADMIN_PASSWORD` The password for the Grafana `admin` user.
- `LOCAL_IP` The IP on which to expose many _security_critical_ things, such as the kubo API. **Make sure you understand the implications of setting this to anything but `127.0.0.1`.**
- `COMPOSE_PROJECT_NAME` The  namespace for `docker compose`. Usually `docker compose` uses the name of the directory for this, but that's not a useful name in our case. It's used to name containers and volumes.

It's easy to supply these via a `.env` file, like so:
```shell
GEOIP_ACCOUNT_ID=<numeric ID>
GEOIP_LICENSE_KEY=<alphanumeric key>
GRAFANA_ADMIN_PASSWORD=<some password>
LOCAL_IP=127.0.0.1
COMPOSE_PROJECT_NAME=ipfs-monitoring
```

### Updating

```shell
# Stop running containers
docker compose down

# Update sources
pushd ..
git pull origin master
popd

# Build new version
docker compose build

# Load COMPOSE_PROJECT_NAME
source .env

# Remove old configuration
docker volume rm ${COMPOSE_PROJECT_NAME}_ipfs_01_path
docker volume rm ${COMPOSE_PROJECT_NAME}_ipfs_02_path

# If Grafana dashboards were updated:
docker volume rm ${COMPOSE_PROJECT_NAME}_grafana_data

# Restart
docker compose up
```

## Components

### Monitors

We're running two monitors using our [plugin](../README.md).
They are using the [Dockerfile](../Dockerfile) in the parent directory, which is a recompiled stock kubo with our plugin.
The nodes are configured via [001_configure_ipfs.sh](./001_configure_ipfs.sh).
The second node is additionally configured using [002_configure_second_daemon_addresses.sh](./002_configure_second_daemon_addresses.sh), which configures the daemon to use different ports.
This is necessary, because the daemon can figure out its own public IP, but apparently not its port, and then announces a wrong port if we only remap it using Docker.

The monitors have a hostname configured, which is used to tag the origin of Bitswap messages reported to RabbitMQ.

### RabbitMQ

RabbitMQ handles the Bitswap realtime monitoring data.
Refer to the [documentation of the plugin](../README.md).
There's some configuration and provisioning going on, via [rabbitmq.conf](rabbitmq.conf), [rabbitmq-definitions.json](rabbitmq-definitions.json), and [rabbitmq.advanced.config](rabbitmq.advanced.config).

### Monitoring Client

This is the [bitswap-monitoring-client](https://github.com/trudi-group/ipfs-tools/tree/master/bitswap-monitoring-client).
It's running the image produced by [this Dockerfile](https://github.com/trudi-group/ipfs-tools/blob/master/Dockerfile.bitswap-monitoring-client).
The configuration is in [monitoring_client_config.yaml](./monitoring_client_config.yaml).

Because GeoIPUpdate updates the GeoIP databases regularly, you'd need to restart this sometimes to pick up the new database.
It's unclear how often geolocation data changes and how necessary this is.
The client is stateless, so restarting it should have minimal impact on the metrics.

The client can log messages to disk.
For this, uncomment the `disk_logging_directory` field in the [monitoring_client_config.yaml](monitoring_client_config.yaml) configuration file.
Also, mount the logging directory by uncommenting the `traces` mount in the `docker-compose.yml` file.
The client changes ownership of the logging directory and log files to the values of `PUID` and `PGID`, configured via environment variables.

### Monitoring Size Estimator

This is the [monitoring-size-estimator](https://github.com/trudi-group/ipfs-tools/tree/master/monitoring-size-estimator).
It's running the image produced by [this Dockerfile](https://github.com/trudi-group/ipfs-tools/blob/master/Dockerfile.monitoring-size-estimator).
The configuration is in [monitoring_size_estimator_config.yaml](./monitoring_size_estimator_config.yaml).

### GeoIPUpdate

This updates the MaxMind GeoLite databases regularly.
Needs the `GEOIP_ACCOUNT_ID` and `GEOIP_LICENSE_KEY` environment variables to work.

### Prometheus, Grafana, etc.

Monitoring and displaying things.
Grafana dashboards are provisioned.
The admin password needs to be supplied via an environment variable.
