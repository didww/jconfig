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

## Install

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

## Try it without a device

`cmd/fakejunos` is a stand-in Junos box that speaks both transports:

```sh
make build fakejunos
./fakejunos -n 2 -dir /tmp/fakejunos          # prints host, port, known_hosts
```

Point a config at the printed ports and run a single pass:

```sh
./jconfig -config jconfig.yml -once
```

## Real devices

jconfig needs an account that may read the configuration over SSH:

```
set system login class config-backup permissions view-configuration
set system login user backup class config-backup
set system login user backup authentication ssh-ed25519 "ssh-ed25519 AAAA..."
set system services ssh
set system services netconf ssh        # only for transport: netconf
```

With `view-configuration` alone the stored config has secrets replaced by
`## SECRET-DATA`, which is usually what you want in a git repository. Add the
`secret` permission bit to the class if you need a config that can be pasted
back verbatim — the encrypted hashes then land in git, so treat the repository
accordingly.

Host keys are verified against `known_hosts` by default. Collect them once:

```sh
ssh-keyscan -H mx1.ams mx2.ams >> /var/lib/jconfig/.ssh/known_hosts
```

Setting `insecure_ignore_host_key: true` disables verification for a device.

## Configure

See [`jconfig.example.yml`](jconfig.example.yml) for an annotated reference. A
minimal configuration:

```yaml
listen: "127.0.0.1:9639"

repo:
  path: /var/lib/jconfig/configs

defaults:
  username: backup
  password: "${JCONFIG_DEVICE_PASSWORD}"
  known_hosts: /var/lib/jconfig/.ssh/known_hosts

devices:
  - name: mx1.ams
    host: 10.0.0.1
    group: core
  - name: mx2.ams
    host: 10.0.0.2
    group: core
    transport: netconf
```

Any `${VAR}` or `${VAR:-default}` is expanded from the environment when the
file is read, so passwords and tokens stay out of the file. `$${VAR}` is a
literal `${VAR}`.

Every field under `defaults:` may be overridden per device; precedence is
`device entry > defaults > built-in`.

### Credentials

A device authenticates with a password, a key file, or an inline key:

```yaml
defaults:
  username: backup
  password: "${JCONFIG_DEVICE_PASSWORD}"   # or
  key_file: /var/lib/jconfig/.ssh/id_ed25519
  key: "${JCONFIG_SSH_KEY}"                # inline, PEM or base64 PEM
```

`key` and `key_file` are mutually exclusive, and an inline key is parsed at
load time, so a malformed one fails `-check` rather than every backup.

Inline keys accept **either a literal PEM block or base64-encoded PEM**. Use
base64 whenever the value arrives through `${VAR}`: expansion happens on the
raw text before the YAML is parsed, so a PEM's newlines would be substituted
into the middle of the document and break it. A literal block is fine when it
is written in the file itself:

```yaml
key: |
  -----BEGIN OPENSSH PRIVATE KEY-----
  ...
  -----END OPENSSH PRIVATE KEY-----
```

`repo.push.key` works the same way for the git remote. Whether a key is needed
there depends on the URL: `https://` remotes use `username`/`password`, a path
or `file://` needs nothing, and `ssh://` or `git@host:path` requires
`repo.push.key` or `repo.push.key_file`.

Unknown fields are rejected at load time, so a typo like `intrval:` is an error
rather than a silently ignored setting.

### Repository layout

`repo.layout: flat` (default) writes one file per device and format:

```
mx1.ams.conf
mx1.ams.set
```

`repo.layout: group` nests them under each device's `group:`:

```
core/mx1.ams.conf
core/mx1.ams.set
edge/mx2.ams.conf
```

If you configure a push remote, create the bare repository with a matching
default branch (`git init --bare --initial-branch=main`), otherwise its `HEAD`
points at a branch jconfig never writes and `git log` on the server looks empty
even though the refs are there.

## Run

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

### The two listeners

jconfig binds two sockets so the port a Prometheus server scrapes cannot also
be used to trigger backups or enumerate the inventory.

`listen` — metrics and probes. Nothing else is served here.

| Endpoint | Purpose |
| --- | --- |
| `GET /metrics` | Prometheus exposition |
| `GET /healthz` | liveness: the process is running |
| `GET /ready` | readiness: the git repository is usable, `503` if not |

`management_listen` — the control surface. Defaults to `127.0.0.1:9640`.

| Endpoint | Purpose |
| --- | --- |
| `GET /` | plain-text status table |
| `GET /devices` | per-device status as JSON |
| `POST /backup` | back up everything now |
| `POST /backup?device=mx1.ams` | back up one device |
| `POST /backup?wait=false` | trigger and return immediately |

The control surface is **unauthenticated**, which is why it is on loopback by
default:

```yaml
management_listen: "127.0.0.1:9640"  # loopback only (default)
management_listen: ""                # disabled entirely
```

A run already in progress returns `409`.

## Kubernetes

The chart is in [`charts/jconfig`](charts/jconfig) and is published to
`oci://ghcr.io/didww/charts/jconfig`. The image is built from the
[`Dockerfile`](Dockerfile): distroless Debian 13, static, no `git` binary
needed at runtime.

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

To log in to devices with a key instead of a password, put the PEM in
`sshPrivateKey` and point the config at it. The chart keeps key material in the
Secret and passes it base64-encoded through the environment, so it never
reaches the ConfigMap:

```yaml
sshPrivateKey: |
  -----BEGIN OPENSSH PRIVATE KEY-----
  ...
  -----END OPENSSH PRIVATE KEY-----
config:
  defaults:
    key: "${JCONFIG_SSH_KEY}"
```

`gitPrivateKey` does the same for an SSH git remote, as `${JCONFIG_GIT_SSH_KEY}`.

`serviceMonitor.enabled` and `prometheusRule.enabled` wire up
prometheus-operator; both are off by default so the chart installs on a
cluster without those CRDs. The PrometheusRule ships the same alerts as
[`deploy/alerts.yml`](deploy/alerts.yml).

The pod is **stateless**. With `repo.clone_on_init: true` (the default whenever
a push remote is configured), jconfig clones `push.url` into an `emptyDir` at
startup, commits into it during the run, and pushes back. A restarted or
rescheduled pod picks the history up again, so there is no PVC and no
StatefulSet to manage. Swap the `emptyDir` for a PVC if you would rather not
re-clone on every restart — keep `strategy: Recreate` either way, since two
pods writing one git history will fight.

What happens on startup:

| Situation | Behaviour |
| --- | --- |
| Volume empty, remote has commits | clone, continue that history |
| Volume empty, remote empty or branch missing | initialise a fresh repository |
| Volume empty, remote unreachable | **startup fails** — a fresh history here would only produce rejected pushes |
| Volume already holds the repository | open it as-is |
| Volume holds unrelated files | startup fails rather than cloning over them |

Set `repo.clone_on_init: false` to always start locally instead.

Probes map to the metrics port: `/healthz` for liveness, `/ready` for
readiness. `/ready` reports `503` while the repository is unusable, so a pod
whose clone failed never goes ready. Because a first clone of a large history
can be slow, the manifest gives it a `startupProbe` with a generous budget.

The management socket stays on loopback inside the pod and is not in the
Service. Reach it with:

```sh
kubectl port-forward deploy/jconfig 9640:9640
curl -XPOST localhost:9640/backup
```

Set `management.enabled=false` to switch the control surface off entirely.

## Metrics

### Is the backup working?

| Metric | Meaning |
| --- | --- |
| `jconfig_backup_success{device}` | 1 if the last attempt succeeded |
| `jconfig_backup_last_success_timestamp_seconds{device}` | when it last worked |
| `jconfig_backup_last_attempt_timestamp_seconds{device}` | when it was last tried |
| `jconfig_backup_last_error{device,stage}` | 1 on the stage that failed; the series disappears once the device recovers |
| `jconfig_backup_errors_total{device,stage}` | failures by stage |
| `jconfig_backup_attempts_total{device,result}` | attempts by outcome |
| `jconfig_backup_consecutive_failures{device}` | how long it has been broken |
| `jconfig_backup_duration_seconds{device}` | duration histogram |
| `jconfig_backup_last_duration_seconds{device}` | duration of the last attempt |
| `jconfig_devices_total`, `jconfig_devices_enabled`, `jconfig_devices_failing` | fleet totals |
| `jconfig_run_total{result}`, `jconfig_run_last_timestamp_seconds`, `jconfig_run_last_duration_seconds`, `jconfig_run_in_progress` | scheduler runs |

`stage` is one of `connect`, `auth`, `fetch`, `parse` or `git`, which separates
"the device is unreachable" from "the credentials are wrong" from "we could not
store what we fetched".

### Is it reaching git?

| Metric | Meaning |
| --- | --- |
| `jconfig_git_operations_total{operation,result}` | `commit`, `push`, `status`, `open`, `clone` by outcome |
| `jconfig_git_last_error_timestamp_seconds{operation}` | when that operation last failed |
| `jconfig_git_commits_total{device}` | commits written per device |
| `jconfig_git_last_commit_timestamp_seconds` | newest commit in the repository |
| `jconfig_git_push_enabled`, `jconfig_git_push_success` | whether pushing is on, and whether the last push worked |
| `jconfig_git_last_push_success_timestamp_seconds` | when the remote last received commits |
| `jconfig_git_unpushed_commits` | commits the remote does not have |
| `jconfig_git_repo_dirty` | 1 if the worktree has uncommitted changes, i.e. a run was interrupted mid-write |

### What is on the devices?

| Metric | Meaning |
| --- | --- |
| `jconfig_device_info{device,host,group,transport,model,os_version}` | inventory and what the device reports |
| `jconfig_device_last_commit_timestamp_seconds{device}` | when the config was last committed **on the device** |
| `jconfig_device_last_commit_by{device,user}` | who committed it |
| `jconfig_config_changed_total{device}` | times the stored config changed |
| `jconfig_config_last_changed_timestamp_seconds{device}` | when it last changed |
| `jconfig_config_bytes{device,format}`, `jconfig_config_lines{device,format}` | size of the last fetch |

`jconfig_device_last_commit_timestamp_seconds` is useful beyond backup health:
it shows where changes are happening, and alerting on a device whose on-box
commit time is newer than `jconfig_config_last_changed_timestamp_seconds`
catches a config that changed but was never captured.

### The exporter itself

| Metric | Meaning |
| --- | --- |
| `jconfig_build_info{version,commit,go_version}` | build metadata, always 1 |
| `jconfig_config_load_success{path}` | 0 after a `SIGHUP` reload was rejected; the previous configuration stays in effect |

### Alerting

Ready-made rules are in [`deploy/alerts.yml`](deploy/alerts.yml), covering
failing and stale backups, auth/connect problems, a fleet-wide failure ratio,
commit and push failures, an unpushed backlog and a dirty repository.

Scrape config:

```yaml
scrape_configs:
  - job_name: jconfig
    static_configs:
      - targets: ["127.0.0.1:9639"]
```

## Notes on behaviour

- **No empty commits.** Fetched configs are compared with what is already in
  the worktree; identical content produces no commit. Line endings, trailing
  whitespace and trailing blank lines are normalised first, and `remove_lines`
  regexps drop volatile lines that would otherwise churn the history.
- **A failed fetch never touches the repository.** Files are only written after
  a device returned a complete, non-empty configuration, so a device that goes
  unreachable keeps its last good config in git rather than losing it.
- **A git failure is a backup failure.** If the config was fetched but could not
  be committed, the device is counted as failed and `stage="git"` is recorded —
  fetching without storing is not a successful backup.
- **Runs do not overlap.** One run executes at a time; a trigger arriving during
  a run gets `409`. Devices inside a run go in parallel up to
  `scheduler.concurrency`.
- **Retry backoff.** A failed device is retried after `scheduler.retry_interval`
  rather than waiting a full interval.

## Testing

```sh
make test     # unit tests plus end-to-end runs against a fake Junos device
make race     # the same under the race detector
```

The test suite starts a real in-process SSH server that speaks the Junos CLI and
NETCONF, so both transports, host-key verification, auth failure, device error
output, commit deduplication and push are exercised without hardware.

## License

GNU General Public License v3.0 or later. See [`LICENSE`](LICENSE).

    Copyright (C) 2026 DIDWW

    This program is free software: you can redistribute it and/or modify it
    under the terms of the GNU General Public License as published by the Free
    Software Foundation, either version 3 of the License, or (at your option)
    any later version.

    This program is distributed in the hope that it will be useful, but
    WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY
    or FITNESS FOR A PARTICULAR PURPOSE. See the GNU General Public License
    for more details.
