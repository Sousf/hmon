# hmon

A terminal dashboard for a small fleet of Linux machines. Polls each host over
SSH and shows the whole fleet in one sortable table.

```
  hmon                                               5 hosts · 09:41:22

  HOST        STATUS   CPU         MEM          /      TEMP    NET ↓ ↑
  nas         ● up     12% ▁▂▁▁▃   8.1/32G      61%    38°C    1.2M 0.3M
  proxmox-1   ● up     47% ▃▄▅▄▆   22.4/64G     34%    52°C    8.4M 2.1M
  pi-dns      ● up      3% ▁▁▁▁▁   0.3/1.0G     22%    44°C    0.1M 0.0M
  media       ● up     88% ▆▇█▇█   14.2/16G     91%    71°C     22M 1.4M
  backup      ○ down    —          —             —      —        —

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

Aliases, `IdentityFile`, ports, and `ProxyJump` all work. Use the mapping form
for more control:

```yaml
hosts:
  - host: media-01.lan
    name: media
    filesystems: [/, /mnt/media]   # omit to report every filesystem
```

Thresholds (`cpu`, `mem`, `disk`, `temp`) set warn/crit colours only — there is
no alerting.

## Keys

| Key | Action |
|---|---|
| `↑` `↓` / `k` `j` | Move selection |
| `enter` / `esc` | Open / leave detail view |
| `s` / `i` | Cycle sort column / invert |
| `c` / `m` | Sort processes by CPU / memory (detail view) |
| `r` / `q` | Refresh / quit |

Detail view adds every filesystem, interface, and sensor, plus top processes
with true instantaneous CPU (sampled from `/proc/<pid>/stat` twice, not `ps`'s
lifetime average).

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
