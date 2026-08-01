# hmon

A terminal dashboard for a small fleet of Linux machines. Polls each host over
SSH and shows the whole fleet in one sortable table.

```
  hmon                                               5 hosts · 09:41:22

  HOST        STATUS   CPU         MEM          /      TEMP    NET ↓ ↑
  nas            ●     12% ▁▂▁▁▃   8.1/32G      61%    38°C    1.2M 0.3M
  proxmox-1      ●     47% ▃▄▅▄▆   22.4/64G     34%    52°C    8.4M 2.1M
  pi-dns         ●      3% ▁▁▁▁▁   0.3/1.0G     22%    44°C    0.1M 0.0M
  media          ●     88% ▆▇█▇█   14.2/16G     91%    71°C     22M 1.4M
  backup         ○    —          —             —      —        —

  ↑↓ move · enter detail · s sort · r refresh · q quit
```

**Nothing is installed on the monitored machines.** The collector is a POSIX
shell script piped to each host's `sh` over stdin on every poll, reading only
`/proc` and `/sys`. Hosts need an SSH login and a shell — no agent, no
`lm-sensors`.

## Install

```sh
brew install Sousf/tap/hmon                        # macOS and Linux
go install github.com/Sousf/hmon/cmd/hmon@latest   # any Go toolchain
```

Prebuilt binaries for darwin/linux on amd64/arm64 are also attached to each
[release](https://github.com/Sousf/hmon/releases).

## Configure

```sh
mkdir -p ~/.config/hmon
hmon -example > ~/.config/hmon/config.yaml
```

Connection details come from `~/.ssh/config`, so a host is usually just a name:

```yaml
hosts:
  - nas
  - proxmox-1
  - pi-dns
```

Aliases, `IdentityFile`, ports, and `ProxyJump` all work.

### Watch lists

Three lists say what to keep an eye on. Each is global, and each can be
overridden per host. All are empty by default, which means no filtering:
report every filesystem, watch no particular service, show every container.

```yaml
filesystems: [/, /mnt/tank] # mount points to report
services: [ssh, docker, postgresql] # systemd units that should be active
containers: [caddy, synapse, postgres] # containers that should be running

hosts:
  - nas
  - host: media-01.lan
    name: media
    filesystems: [/, /mnt/media] # per-host override
    services: [jellyfin]
    containers: [sonarr, radarr]
```

`services` matters because **failed and not-running are different things**. A
unit someone stopped, or that never came up after a reboot, is `inactive`
rather than `failed`, so the failed-unit check alone reports a clean host. A
watched unit that does not exist is shown as `n/a`, not as an outage — that is
a config mismatch, not a problem with the machine.

A watched container that is absent entirely is reported as `missing`.

Thresholds (`cpu`, `mem`, `disk`, `temp`) set warn/crit colours only — there is
no alerting.

### Guests

LXD instances are discovered automatically and get a row of their own, nested
under the machine running them:

```
  hermes3         ●     1d 2h    5% ▂▁▂▂   4.2G/15.4G   51%   44°C   ↓210K ↑ 88K
  ├─ buildbox ct  ○ stopped         —      —             —      —    —
  └─ nixos vm     ●       47m    1% ▁▁▁▁   396M/4.0G    19%    n/a   ↓  1K ↑  2K
```

Nothing to configure — they are found, not listed. Set `guests: false` to hide
them.

A guest is measured **from the inside**: the same collector is piped onward
into each instance over `lxc exec`, so CPU, memory, load, disk, and network
come from the guest's own `/proc` rather than from what the daemon can see
outside it. A guest with no `lxd-agent`, or one that is stopped, falls back to
LXD's own accounting and shows dashes where it has nothing to report.

A stopped instance reads `stopped`, not `down`, and counts as healthy — you
turned it off on purpose. Guests are read-only: `x`, `X`, `R`, and `space`
apply to machines only, since all of them go over SSH to an address a guest
does not have.

## Keys

| Key               | Action                                               |
| ----------------- | ---------------------------------------------------- |
| `↑` `↓` / `k` `j` | Move selection                                       |
| `enter` / `esc`   | Open / leave detail view                             |
| `s` / `i`         | Cycle sort column / invert                           |
| `c` / `m`         | Sort processes by CPU / memory                       |
| `S`               | Open an interactive ssh session on the selected host |
| `R`               | Reboot the selected host (asks first)                |
| `r` / `q`         | Refresh / quit                                       |

The layout is responsive. On a tall terminal the space below the table shows
live detail for the selected host — filesystems, interfaces, sensors, and top
processes — so arrow keys sweep the fleet without pressing anything. On a short
terminal it stays a compact table, and `enter` opens the full-screen detail
view.

Process CPU is a true instantaneous measurement, sampled from `/proc/<pid>/stat`
twice and diffed, rather than `ps`'s lifetime average.

`S` hands the terminal to `ssh` for the selected host and takes it back when
you exit, refreshing on return.

`R` reboots the selected host. It is the only thing hmon does that changes a
machine rather than reading one, so it never happens on a single keystroke: a
full-screen prompt names the host, spells out the exact command, and warns if
the host is already unreachable. Only `y` proceeds — every other key cancels.

The reboot runs as `ssh -t <host> sudo systemctl reboot`, interactively, so
`sudo` can prompt for a password. Running it in the background would fail
silently on any host without passwordless sudo.

## Running commands

`x` runs a command across hosts. `space` marks hosts and `a` marks all; with
nothing marked it runs on the selected host alone — an empty mark set means
"just this one", never "all of them". The prompt lists the target hosts while
you type, because the mistake worth preventing is running somewhere you did
not mean to.

Commands run in parallel and their output is collected into a scrollable view
with an exit code per host, so results are comparable side by side.

`@path` pipes a local script file instead:

```
> @./checks/disk.sh
```

The file is read locally and piped to `sh` on each host, the same mechanism the
collector uses — nothing is written to the monitored machines.

Execution is non-interactive, so **anything that prompts will fail rather than
hang**. That is deliberate: a prompt nobody can answer would block the whole
fan-out. `command_timeout` bounds each run, defaulting to 60s.

### Running as root

`X` runs the command as root. hmon asks for your sudo password at a masked
prompt, then runs the whole script under `sudo` on every target in parallel.

The script runs as root from the first line, so it does not need `sudo`
sprinkled through it — and however many privileged commands it contains, you
enter one password.

How the password is handled:

- Typed at a masked prompt; only its length is ever drawn.
- Sent as the **first line of stdin** over the existing encrypted SSH
  connection. `sudo -S` consumes exactly that line and the shell receives the
  rest as the script.
- **Never placed in the command line**, where any user on the host could read
  it out of `ps`. Never written to disk. Never echoed into the results.
- Kept only for the run that uses it, then dropped — you are asked again next
  time. (`sudo` on the host caches its own credentials for ~15 minutes, so
  repeats are cheap on that side.)

A rejected password is reported as `sudo password rejected`, and the script
does not run.

Use `S` instead when you need a real terminal on one host — anything that
prompts interactively still belongs there.

## Health

Resource metrics cannot tell you a service died — a host with a crashed
postgres looks perfectly healthy on CPU and memory. hmon flags conditions no
column can express, against the host name:

| Flag | Meaning                                                                  |
| ---- | ------------------------------------------------------------------------ |
| `✗`  | A watched service or container is down, or a systemd unit has failed    |
| `!`  | Reboot required (`/var/run/reboot-required`)                            |

## Scripting

```sh
hmon -once           # one poll, one line per host, no TUI
hmon -once -json     # the same as JSON
```

This is how hmon stays a dashboard instead of becoming a service. Rather than
growing notifications and a daemon, run it from cron and pipe it into whatever
alerting you already have:

```sh
hmon -once -json | jq -r '.hosts[] | select(.healthy | not) | .name'
```

Each host carries a `healthy` boolean, false when the host is not up, a watched
service or container is not running, or a unit has failed. Both modes poll
twice about a second apart, because CPU and the network and disk rates are
derived by diffing two samples.

## Notes

- Trend history is a 60-sample in-memory ring buffer; no database, resets on quit.
- One failed poll marks a host stale and keeps its last values; two mark it down.
- Auth failures and unparseable output are shown distinctly from unreachable.
- Reboot counter resets produce no reading rather than a fictional spike.

## Development

```sh
go test ./...
```

No test touches the network. The collector can't run natively on macOS (no
`/proc`); exercise it with:

```sh
docker run --rm -i alpine:3.20 sh -s < internal/collect/collector.sh
docker run --rm -i debian:bookworm-slim sh -s procs < internal/collect/collector.sh
```

Both matter — busybox awk and mawk differ in ways that have caused real bugs.

## License

MIT
