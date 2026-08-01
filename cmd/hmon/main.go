// Command hmon is a terminal dashboard for a small fleet of Linux machines.
//
// It polls each host over SSH and renders their vitals in a sortable table.
// Nothing is installed on the monitored machines: the collector is piped to
// their shell over stdin on every poll.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sousf/hmon/internal/collect"
	"github.com/Sousf/hmon/internal/config"
	"github.com/Sousf/hmon/internal/model"
	"github.com/Sousf/hmon/internal/report"
	"github.com/Sousf/hmon/internal/ui"
)

const exampleConfig = `# ~/.config/hmon/config.yaml
#
# Connection details come from ~/.ssh/config, so a host is usually just a name.

# How often to poll every host.
interval: 2s

# Per-host deadline. A host slower than the interval simply updates less
# often; it is not marked down for being slow.
timeout: 5s

# Deadline for a command run across hosts with x. Generous compared to the
# poll timeout: a poll must fail fast so one slow host does not stall the
# table, but a command is something you are deliberately waiting on.
command_timeout: 60s

hosts:
  - nas
  - proxmox-1
  - pi-dns

  # Use the mapping form when a host needs more than a name. Any of the three
  # watch lists below can be overridden per host.
  # - host: media-01.lan
  #   name: media
  #   filesystems: [/, /mnt/media]
  #   services: [jellyfin]
  #   containers: [sonarr, radarr]

# Which mount points to report. Empty means every real filesystem.
filesystems: []

# systemd units to watch. This catches a service that was stopped rather than
# one that crashed — systemd does not consider a stopped unit failed, so the
# failed-unit check alone would show a clean host.
services: []

# Containers to watch, by name. A watched container that does not exist at all
# is reported as missing. Empty means show every container.
containers: []

# LXD instances get a row of their own, nested under the machine running them,
# and are measured from the inside — so a VM shows real CPU, memory, and disk
# rather than whatever the daemon can see from outside. Nothing to configure:
# they are discovered, not listed. Set false to hide them.
guests: true

# Thresholds only choose the colour a value is drawn in.
thresholds:
  cpu:  {warn: 75, crit: 90}
  mem:  {warn: 80, crit: 92}
  disk: {warn: 80, crit: 90}
  temp: {warn: 65, crit: 80}
`

// Build metadata, injected at release time via -ldflags. The defaults are what
// you get from a plain `go build`, which is honest about being an untagged
// local build rather than claiming a version it does not have.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hmon:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("c", config.DefaultPath(), "path to config file")
	printExample := flag.Bool("example", false, "print an example config and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	once := flag.Bool("once", false, "poll once, print the fleet, and exit (no TUI)")
	jsonOut := flag.Bool("json", false, "with -once, print JSON instead of a table")
	flag.Parse()

	if *showVersion {
		fmt.Printf("hmon %s (commit %s, built %s)\n", version, commit, date)
		return nil
	}

	if *printExample {
		fmt.Print(exampleConfig)
		return nil
	}

	cfg, err := config.Load(*cfgPath)
	if errors.Is(err, config.ErrNotFound) {
		// A missing config is the expected first-run state, so answer it with
		// something the user can act on rather than an opaque error.
		// Both paths are quoted: they are meant to be copied straight into a
		// shell, and an unquoted path containing a space silently writes the
		// file somewhere else.
		fmt.Fprintf(os.Stderr, "No config found at %s\n\n", *cfgPath)
		fmt.Fprintf(os.Stderr, "Create it with:\n\n")
		fmt.Fprintf(os.Stderr, "    mkdir -p %q\n", filepath.Dir(*cfgPath))
		fmt.Fprintf(os.Stderr, "    hmon -example > %q\n\n", *cfgPath)
		fmt.Fprintf(os.Stderr, "Then edit it to list your machines.\n")
		os.Exit(1)
	}
	if err != nil {
		return err
	}

	fleet := model.NewFleet(cfg.HostRefs())
	runner := collect.NewSSHRunner(cfg.Timeout)
	poller := &collect.Poller{Runner: runner, Timeout: cfg.Timeout}
	// Ad-hoc commands get their own runner with a longer deadline: polls are
	// tuned to fail fast, but a command is something the operator is waiting on.
	executor := collect.NewExecRunner(cfg.CommandTimeout)

	// Tear down multiplexed SSH masters on exit so quitting does not leave
	// background ssh processes behind.
	defer closeAll(runner, fleet)

	if *once {
		return runOnce(fleet, poller, cfg.GuestsEnabled(), *jsonOut)
	}

	p := tea.NewProgram(
		ui.New(cfg, fleet, poller, executor),
		tea.WithAltScreen(),
	)
	_, err = p.Run()
	return err
}

func closeAll(runner *collect.SSHRunner, fleet *model.Fleet) {
	for _, h := range fleet.Hosts {
		runner.Close(h.Addr)
	}
}

// runOnce polls the fleet without a TUI and prints the result.
//
// It polls twice, a second apart, because CPU percentage and the network and
// disk rates are all derived by diffing two samples — a single poll would
// report the host but omit exactly the numbers most worth alerting on.
func runOnce(fleet *model.Fleet, poller *collect.Poller, guests, asJSON bool) error {
	poll := func(detail bool) {
		// Guests are discovered by the first poll and appear in the fleet
		// partway through this run, so the fan-out is taken over machines only.
		// They have no address to connect to in any case: their readings arrive
		// on their host's poll.
		var machines []*model.Host
		for _, h := range fleet.Hosts {
			if !h.IsGuest() {
				machines = append(machines, h)
			}
		}

		var wg sync.WaitGroup
		results := make([]struct {
			name string
			s    model.Sample
			err  error
		}, len(machines))

		for i, h := range machines {
			wg.Add(1)
			go func(i int, h *model.Host) {
				defer wg.Done()
				results[i].name = h.Name
				results[i].s, results[i].err = poller.Poll(
					context.Background(), h.Addr,
					collect.Opts{
						Detail:     detail,
						Services:   h.Services,
						Containers: len(h.Containers) > 0,
						Guests:     guests,
					})
			}(i, h)
		}
		wg.Wait()

		// Applied after the fan-out completes, on this goroutine only, so the
		// fleet keeps its single-writer property.
		for _, r := range results {
			if r.err != nil {
				fleet.Fail(r.name, collect.Classify(r.err), r.err.Error())
				continue
			}
			fleet.Apply(r.name, r.s)
		}
	}

	// The first poll exists only to supply counters for the second to diff
	// against, so it skips the expensive sections. That matters on a cold run:
	// with no multiplexed connection yet, a full detail poll can exceed the
	// timeout, and then the surviving sample has nothing to diff against and
	// the report silently omits every rate.
	poll(false)
	time.Sleep(time.Second)
	poll(true)

	if asJSON {
		return report.Write(os.Stdout, report.Build(fleet, time.Now()))
	}
	printPlain(os.Stdout, fleet)
	return nil
}

// printPlain writes a terse line per host, for eyeballing from a script or a
// terminal without a TTY.
func printPlain(w io.Writer, fleet *model.Fleet) {
	for _, h := range fleet.Hosts {
		status := h.Status.String()
		cpu := "—"
		if h.HasCPUPct {
			cpu = fmt.Sprintf("%.0f%%", h.CPUPct)
		}
		mem := "—"
		if h.Cur.HasMem {
			mem = fmt.Sprintf("%.0f%%", h.Cur.MemPct())
		}

		var notes []string
		if n := len(h.Cur.FailedUnits); n > 0 {
			notes = append(notes, fmt.Sprintf("%d failed unit(s)", n))
		}
		for _, s := range h.Cur.StoppedServices() {
			notes = append(notes, s.Name+" "+s.ActiveState)
		}
		for _, c := range h.Cur.StoppedContainers() {
			notes = append(notes, c.Name+" "+c.State)
		}
		if h.Cur.RebootRequired {
			notes = append(notes, "reboot required")
		}

		line := fmt.Sprintf("%-16s %-8s cpu %-5s mem %-5s", h.Name, status, cpu, mem)
		if len(notes) > 0 {
			line += "  " + strings.Join(notes, "; ")
		}
		fmt.Fprintln(w, line)
	}
}
