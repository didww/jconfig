# jconfig

Junos configuration backup into git, as a single static Go binary.

Like [oxidized](https://github.com/ytti/oxidized), but deliberately narrow: it
backs up **Junos only**, stores what the device has **committed** into **git**,
and ships a **Prometheus exporter** built for answering two questions —
*is every device still being backed up?* and *is every backup actually reaching
git?*

- One binary, no Ruby, no runtime dependencies. Git is spoken natively through
  [go-git](https://github.com/go-git/go-git); the `git` binary is not required.
- Two transports, selectable per device: **SSH CLI** (`show configuration`) or
  **NETCONF** (`<get-configuration database="committed">`).
- Stores the curly-brace config (`.conf`) and the `display set` form (`.set`);
  `display xml` is available too.
- Commits only when the configuration actually changed, one commit per device,
  with the device's model, Junos release and on-box commit metadata in the
  commit message.
- Optional push to a remote, with push failures and unpushed-commit backlog
  exposed as metrics.
- Built-in scheduler with per-device intervals, plus an HTTP endpoint to force
  a run.
- Two separate listeners: a metrics socket safe to expose to Prometheus, and a
  loopback-only management socket for the control surface.
- Container-ready: a pod starting on a blank volume clones the configured
  remote, so history continues instead of forking.

## Install and run

Container image (distroless Debian 13, static, multi-arch):

```sh
docker pull ghcr.io/didww/jconfig:latest
```

Helm chart, published as an OCI artifact:

```sh
helm install jconfig oci://ghcr.io/didww/charts/jconfig \
  --version 0.1.0 -f my-values.yaml
```

From source:

```sh
make build                 # ./jconfig for this host
make dist                  # cross-compiled binaries in ./dist
make image                 # container image
```

Go 1.25 or newer (the floor comes from `go-git`, `client_golang` and
`x/crypto`). `CGO_ENABLED=0`, so the result is fully static.

```sh
jconfig -config /etc/jconfig/jconfig.yml            # daemon: scheduler + HTTP
jconfig -config /etc/jconfig/jconfig.yml -once      # single pass, then exit
jconfig -config /etc/jconfig/jconfig.yml -check     # validate and exit
```

`-once` exits non-zero if any device failed or the push failed, which makes it
usable straight from cron. Add `-metrics-file /var/lib/node_exporter/jconfig.prom`
to publish metrics through the node_exporter textfile collector instead of
running the daemon.

`SIGHUP` reloads the configuration without dropping state:
devices that disappeared have their metrics removed, new devices are scheduled,
and a configuration that fails to load leaves the running one untouched (and
sets `jconfig_config_load_success` to 0).

A systemd unit is in [`deploy/jconfig.service`](deploy/jconfig.service).

## Junos Configuration

```
set system login class config-backup permissions [ view view-configuration ]
set system login user backup class config-backup
set system login user backup authentication ssh-ed25519 "ssh-ed25519 AAAA..."
set system services ssh
set system services netconf ssh        # only for transport: netconf
```

```sh
ssh-keyscan -H mx1.ams mx2.ams >> /var/lib/jconfig/.ssh/known_hosts
```

## Configure

See [`jconfig.example.yml`](jconfig.example.yml) for an annotated reference.

## Kubernetes

The chart is in [`charts/jconfig`](charts/jconfig), published as an OCI
artifact:

```sh
helm install jconfig oci://ghcr.io/didww/charts/jconfig \
  --version 0.1.0 -f my-values.yaml
```

Minimal values:

```yaml
knownHosts: |
  [10.0.0.1]:22 ssh-ed25519 AAAAC3Nz...
secret:
  data:
    JCONFIG_DEVICE_PASSWORD: "..."
    JCONFIG_GIT_TOKEN: "..."
config:
  repo:
    push:
      url: https://git.example.net/noc/junos-configs.git
      username: jconfig
  devices:
    - name: mx1.ams
      host: 10.0.0.1
      group: core
```

Everything else — SSH keys, persistence, probes, and the ServiceMonitor,
PodMonitor and PrometheusRule — is in
[`charts/jconfig/values.yaml`](charts/jconfig/values.yaml).

## Metrics

Metric reference, scrape config and alerting rules are in
[`Prometheus.md`](Prometheus.md).
