#!/bin/sh
# Remote collector for hmon.
#
# This script is never installed on the target: it is piped to `sh -s` over
# stdin on every poll, so hosts need nothing beyond a POSIX shell and awk.
#
# Everything is read from /proc and /sys, which means lm-sensors and any other
# userspace tooling is optional — the kernel exposes these files regardless.
#
# Output is line-oriented "key value..." rather than JSON, because the remote
# side is guaranteed to have `sh` but not `jq`, and emitting correct JSON from
# pure shell invites quoting bugs. Unknown keys are ignored by the parser, so
# new metrics can be added without breaking older builds.
#
# Arguments select the optional, more expensive sections:
#
#   procs          top processes; costs ~0.5s for two /proc samples, so it is
#                  only requested for the host being viewed in detail
#   containers     docker and lxc listings; shells out, so also detail-only
#   svc=a,b,c      report whether these services are active; one systemctl
#                  call, cheap enough to run for the whole fleet every poll

set -u

# A non-interactive ssh shell gets a minimal PATH that often omits the sbin
# directories, which is where lxc and friends live on Debian and Ubuntu. Append
# rather than replace so a host's own PATH still wins.
PATH="$PATH:/usr/local/sbin:/usr/local/bin:/usr/sbin:/sbin:/snap/bin"
export PATH

# Sysfs root, overridable purely so the temperature logic can be exercised
# against a fixture tree in tests — containers mount /sys read-only, so there is
# no other way to reach that branch. Always /sys in real use.
SYSFS="${HMON_SYSFS:-/sys}"

WANT_PROCS=0
WANT_CONTAINERS=0
WATCH_SERVICES=""
for arg in "$@"; do
  case "$arg" in
    procs)      WANT_PROCS=1 ;;
    containers) WANT_CONTAINERS=1 ;;
    svc=*)      WATCH_SERVICES=$(echo "$arg" | cut -d= -f2- | tr ',' ' ') ;;
  esac
done

# Format version. The parser rejects output it does not recognise rather than
# silently misreading columns.
echo "v 1"

# --- uptime ----------------------------------------------------------------
if [ -r /proc/uptime ]; then
  read -r up _ < /proc/uptime
  echo "uptime $up"
fi

# --- cpu -------------------------------------------------------------------
# Raw jiffies. Percentages are computed on the client by diffing consecutive
# polls, which keeps this script stateless.
if [ -r /proc/stat ]; then
  awk '/^cpu /{print "cpu", $2, $3, $4, $5, $6, $7, $8, ($9==""?0:$9); exit}' /proc/stat
fi

# --- memory ----------------------------------------------------------------
# MemAvailable is the honest basis for "used": it accounts for reclaimable
# cache, unlike MemFree which makes every healthy Linux box look full.
#
# Values are emitted in kilobytes exactly as /proc/meminfo reports them, and
# scaled to bytes by the client. Multiplying here would push the result past
# int32 on any machine with more than 2GB, and mawk (the default awk on Debian
# and Ubuntu) switches to %.6g scientific notation at that point — producing
# "mem 8.21706e+09", which no integer parser will accept.
if [ -r /proc/meminfo ]; then
  awk '
    /^MemTotal:/     {total=$2}
    /^MemAvailable:/ {avail=$2; found=1}
    /^MemFree:/      {free=$2}
    /^SwapTotal:/    {swaptotal=$2}
    /^SwapFree:/     {swapfree=$2}
    END {
      if (total + 0 > 0) {
        if (!found) avail = free   # pre-3.14 kernels lack MemAvailable
        printf "mem %.0f %.0f\n", total, avail
      }
      # A host that is swapping is in trouble well before its memory
      # percentage looks alarming, so this is worth reporting separately.
      if (swaptotal + 0 > 0) printf "swap %.0f %.0f\n", swaptotal, swapfree
    }' /proc/meminfo
fi

# --- load ------------------------------------------------------------------
# Core count travels with the load average because load is meaningless without
# it: 4.0 is idle on a 16-core box and a crisis on a dual-core Pi.
if [ -r /proc/loadavg ]; then
  read -r l1 l5 l15 _ < /proc/loadavg
  echo "load $l1 $l5 $l15"
fi
cores=$(nproc 2>/dev/null || grep -c '^processor' /proc/cpuinfo 2>/dev/null || echo 0)
[ "$cores" -gt 0 ] 2>/dev/null && echo "cores $cores"

# --- failed services -------------------------------------------------------
# Resource metrics cannot tell you a service died: a host with a crashed
# postgres looks perfectly healthy on CPU and memory. Names are emitted one
# per line, with the count first so the client can show a health indicator
# without parsing every name.
if command -v systemctl >/dev/null 2>&1; then
  failed=$(systemctl list-units --state=failed --no-legend --plain --no-pager 2>/dev/null \
             | awk '{print $1}' | grep -v '^$')
  if [ -n "$failed" ]; then
    printf 'failedcount %s\n' "$(printf '%s\n' "$failed" | wc -l | tr -d ' ')"
    printf 'failed %s\n' $failed
  else
    echo "failedcount 0"
  fi
fi

# --- watched services ------------------------------------------------------
# "failed" and "not running" are different things: a service someone stopped,
# or that never came up after a reboot, is inactive rather than failed, and the
# failed-unit check above sees nothing wrong. These are the units the operator
# actually cares about, so their state is reported explicitly.
#
# LoadState is reported alongside ActiveState because `is-active` alone cannot
# tell "stopped" from "no such unit" — both come back as inactive. Without the
# distinction, watching a service that actually runs in a container (or is
# named differently, like lxd's snap.lxd.daemon) would show a permanent red
# alarm that no amount of starting things could clear.
#
# One systemctl call handles the whole list. Its output is a blank-line
# separated block per unit, which awk reads in paragraph mode: field 1 is
# LoadState, field 2 ActiveState, paired back to names by record number.
if [ -n "$WATCH_SERVICES" ] && command -v systemctl >/dev/null 2>&1; then
  # shellcheck disable=SC2086
  systemctl show -p LoadState -p ActiveState --value $WATCH_SERVICES 2>/dev/null \
    | awk -v names="$WATCH_SERVICES" '
        BEGIN { RS = ""; n = split(names, arr, " ") }
        NR <= n { print "svc", $1, $2, arr[NR] }'
fi

# --- containers ------------------------------------------------------------
# Stopped containers are included deliberately: one that should be running and
# is not is exactly what this is for. Unlike everything else here this shells
# out to a CLI, so it degrades to nothing when the binary is absent or the
# user is not in the right group.
if [ "$WANT_CONTAINERS" -eq 1 ]; then
  have_runtime=0
  if command -v docker >/dev/null 2>&1; then
    have_runtime=1
    docker ps -a --no-trunc --format '{{.State}} {{.Names}}' 2>/dev/null \
      | head -n 40 | awk 'NF >= 2 { print "container docker", $0 }'
  fi
  if command -v lxc >/dev/null 2>&1; then
    have_runtime=1
    lxc list --format csv -c ns 2>/dev/null \
      | head -n 40 | awk -F, 'NF >= 2 { print "container lxc", tolower($2), $1 }'
  fi

  # Emitted even when nothing was found, so the client can tell "this host has
  # no containers" from "containers were never asked about". Without it, a poll
  # that skipped this section looks identical to one where every watched
  # container has vanished.
  [ "$have_runtime" -eq 1 ] && echo "containersreported 1"
fi

# --- reboot required -------------------------------------------------------
[ -f /var/run/reboot-required ] && echo "rebootrequired 1"

# --- disk i/o --------------------------------------------------------------
# Cumulative sector counts; rates are derived client-side like the network
# counters. Field 3 is the device name, 6 is sectors read, 10 sectors written.
# Sectors are always 512 bytes here regardless of physical block size.
#
# Partitions and virtual devices are skipped so a disk is not counted twice —
# reads on /dev/sda1 also appear on /dev/sda.
if [ -r /proc/diskstats ]; then
  awk '{
    dev = $3
    if (dev ~ /^(loop|ram|dm-|sr|fd)/) next
    if (dev ~ /[0-9]+$/ && dev ~ /^(sd|vd|hd)/) next   # partitions of sd/vd/hd
    if (dev ~ /^nvme[0-9]+n[0-9]+p[0-9]+$/) next       # nvme partitions
    if ($6 + $10 == 0) next                            # never used
    print "disk", dev, $6, $10
  }' /proc/diskstats
fi

# --- filesystems -----------------------------------------------------------
# -P forces POSIX output so column positions are stable across df variants;
# -k forces 1K blocks so the units are never in question. Used is reported
# alongside total and available because total-minus-available would count
# root-reserved blocks and disagree with df's own Use%.
df -Pk 2>/dev/null | awk '
  NR > 1 &&
  $1 !~ /^(tmpfs|devtmpfs|overlay|none|udev|squashfs)$/ &&
  $6 !~ /^\/(proc|sys|dev|run|snap)(\/|$)/ {
    print "fs", $6, $2, $3, $4
  }'

# --- network ---------------------------------------------------------------
# Cumulative byte counters; rates are derived client-side. Loopback is skipped
# because its throughput says nothing about the machine.
#
# Interface names are right-padded into a fixed-width field, so short names like
# "eth0" carry leading whitespace while long ones do not. Splitting on
# whitespace therefore shifts columns for some interfaces and not others;
# anchor on the ":" separator instead.
if [ -r /proc/net/dev ]; then
  awk 'NR > 2 {
    line = $0
    sub(/^[ \t]+/, "", line)
    colon = index(line, ":")
    if (colon == 0) next
    iface = substr(line, 1, colon - 1)
    n = split(substr(line, colon + 1), f, " ")
    if (iface == "lo" || n < 9) next
    print "net", iface, f[1], f[9]
  }' /proc/net/dev | while read -r _ iface rx tx; do
    # Skip interfaces that are not actually up. Without this, every loaded
    # tunnel module (gretap0, erspan0, ip_vti0, ...) shows up as a permanently
    # idle NIC and buries the interfaces you care about.
    state=""
    [ -r "$SYSFS/class/net/$iface/operstate" ] && read -r state < "$SYSFS/class/net/$iface/operstate"
    case "$state" in
      down|"") continue ;;
    esac

    # Skip container and bridge plumbing. On any host running Docker these
    # carry the same packets as the real uplink, so including them counts the
    # same traffic two or three times and swamps the physical interface.
    case "$iface" in
      veth*|br-*|docker*|virbr*|dummy*|bond*.*) continue ;;
    esac

    echo "net $iface $rx $tx"
  done
fi

# --- temperatures ----------------------------------------------------------
# Read hwmon sysfs directly so no lm-sensors install is required. Values are
# millidegrees C. Hosts exposing no sensors simply emit nothing here and render
# as n/a rather than failing the poll.
for f in "$SYSFS"/class/hwmon/hwmon*/temp*_input; do
  [ -r "$f" ] || continue
  read -r milli < "$f" 2>/dev/null || continue
  [ -n "$milli" ] || continue

  dir=$(dirname "$f")
  base=$(basename "$f" _input)
  label=""
  [ -r "$dir/${base}_label" ] && read -r label < "$dir/${base}_label" 2>/dev/null
  if [ -z "$label" ] && [ -r "$dir/name" ]; then
    read -r label < "$dir/name" 2>/dev/null
  fi
  [ -n "$label" ] || label="$base"

  # Value first, label last: labels like "Package id 0" contain spaces, so the
  # free-text field has to be terminal for the line to stay parseable.
  awk -v lbl="$label" -v m="$milli" 'BEGIN { printf "temp %.1f %s\n", m / 1000, lbl }'
done

# Thermal zones cover SoCs (Raspberry Pi and friends) that expose no hwmon.
for f in "$SYSFS"/class/thermal/thermal_zone*/temp; do
  [ -r "$f" ] || continue
  read -r milli < "$f" 2>/dev/null || continue
  [ -n "$milli" ] || continue

  dir=$(dirname "$f")
  label=""
  [ -r "$dir/type" ] && read -r label < "$dir/type" 2>/dev/null
  [ -n "$label" ] || label="thermal"

  awk -v lbl="$label" -v m="$milli" 'BEGIN { printf "temp %.1f %s\n", m / 1000, lbl }'
done

# --- processes (opt-in) ----------------------------------------------------
# ps reports %CPU averaged over each process's entire lifetime, which is
# actively misleading for monitoring: something that pegged a core an hour ago
# still looks busy. Instead, sample /proc/<pid>/stat twice and diff utime+stime
# to get genuine instantaneous usage.
if [ "$WANT_PROCS" -eq 1 ]; then
  # One awk over every stat file at once. Forking awk per process instead —
  # the obvious way to write this — costs several hundred forks per snapshot on
  # a host running containers, which stretches the sampling window to over a
  # second and puts real load on the machine being measured.
  snapshot() {
    awk '{
      # comm is parenthesised and may itself contain spaces or brackets, so
      # anchor on the LAST ")" rather than splitting on whitespace.
      line = $0
      close_paren = 0
      for (i = length(line); i > 0; i--) {
        if (substr(line, i, 1) == ")") { close_paren = i; break }
      }
      if (close_paren == 0) next
      pid  = substr(line, 1, index(line, " ") - 1)
      comm = substr(line, index(line, "(") + 1, close_paren - index(line, "(") - 1)
      rest = substr(line, close_paren + 2)
      n = split(rest, f, " ")
      # After state (f[1]), utime is field 12 and stime 13 of the remainder;
      # rss (in pages) is field 22.
      if (n < 22) next
      print pid, f[12] + f[13], f[22], comm
    }' /proc/[0-9]*/stat 2>/dev/null
  }

  pagesize=$(getconf PAGESIZE 2>/dev/null || echo 4096)
  hz=$(getconf CLK_TCK 2>/dev/null || echo 100)

  # Measure the sampling window from /proc/uptime rather than assuming the
  # sleep duration. `sleep 0.5` is not POSIX, so a strict shell falls back to a
  # full second, and the snapshots themselves take non-zero time; deriving
  # elapsed from the clock keeps the percentages correct either way.
  #
  # The window length sets the resolution: at 100 ticks/second, 0.5s means one
  # tick reads as 2%. A shorter window would be snappier but would quantise
  # every barely-active process up to 5%, which reads as busier than it is.
  read -r t0 _ < /proc/uptime
  first=$(snapshot)
  sleep 0.5 2>/dev/null || sleep 1
  read -r t1 _ < /proc/uptime
  second=$(snapshot)

  procs=$(printf '%s\n---\n%s\n' "$first" "$second" | awk \
    -v pagesize="$pagesize" -v hz="$hz" -v t0="$t0" -v t1="$t1" '
    BEGIN {
      elapsed = t1 - t0
      if (elapsed <= 0) elapsed = 0.2
    }
    /^---$/ { phase = 1; next }
    phase == 0 { before[$1] = $2; next }
    {
      pid = $1
      if (!(pid in before)) next
      ticks = $2 - before[pid]
      if (ticks < 0) next          # pid was reused between samples
      pct = ticks / (hz * elapsed) * 100
      # Command name is emitted last and unescaped: it may contain spaces, and
      # substituting them would corrupt names that legitimately contain
      # underscores (kworker/0:1H-events_highpri, for one).
      comm = $4
      for (i = 5; i <= NF; i++) comm = comm " " $i
      printf "proc %s %.1f %.0f %s\n", pid, pct, $3 * pagesize / 1024, comm
    }')

  # Emit the union of the top processes by CPU and by memory. The client can
  # toggle between those sortings, so sending only one ranking would leave the
  # other view showing the wrong processes. Duplicates are removed by PID
  # client-side. Held in a variable rather than a temp file so the collector
  # never writes to the target's filesystem.
  printf '%s\n' "$procs" | sort -k3 -rn | head -n 15
  printf '%s\n' "$procs" | sort -k4 -rn | head -n 15
fi

echo "end"
