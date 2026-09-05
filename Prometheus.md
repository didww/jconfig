# Prometheus

jconfig exports two things worth alerting on: *is every device still being
backed up?* and *is every backup actually reaching git?* The metrics below are
served on `listen` (`/metrics` by default).
## Is the backup working?

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

## Is it reaching git?

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

## What is on the devices?

| Metric | Meaning |
| --- | --- |
| `jconfig_device_info{device,host,group,vendor,transport,model,os_version}` | inventory and what the device reports |
| `jconfig_device_last_commit_timestamp_seconds{device}` | when the config was last committed **on the device** |
| `jconfig_device_last_commit_by{device,user}` | who committed it |
| `jconfig_config_changed_total{device}` | times the stored config changed |
| `jconfig_config_last_changed_timestamp_seconds{device}` | when it last changed |
| `jconfig_config_bytes{device,format}`, `jconfig_config_lines{device,format}` | size of the last fetch |

`jconfig_device_last_commit_timestamp_seconds` is useful beyond backup health:
it shows where changes are happening, and alerting on a device whose on-box
commit time is newer than `jconfig_config_last_changed_timestamp_seconds`
catches a config that changed but was never captured.

## The exporter itself

| Metric | Meaning |
| --- | --- |
| `jconfig_build_info{version,commit,go_version}` | build metadata, always 1 |
| `jconfig_config_load_success{path}` | 0 after a `SIGHUP` reload was rejected; the previous configuration stays in effect |

## Alerting

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
