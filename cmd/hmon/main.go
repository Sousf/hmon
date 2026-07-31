// Command hmon is a terminal dashboard for a small fleet of Linux machines.
//
// It polls each host over SSH and renders their vitals in a sortable table.
// Nothing is installed on the monitored machines: the collector is piped to
// their shell over stdin on every poll.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Sousf/hmon/internal/collect"
	"github.com/Sousf/hmon/internal/config"
	"github.com/Sousf/hmon/internal/model"
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

hosts:
  - nas
  - proxmox-1
  - pi-dns

  # Use the mapping form when a host needs more than a name:
  # - host: media-01.lan
  #   name: media
  #   filesystems: [/, /mnt/media]   # omit to report every filesystem

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

	// Tear down multiplexed SSH masters on exit so quitting does not leave
	// background ssh processes behind.
	defer func() {
		for _, h := range fleet.Hosts {
			runner.Close(h.Addr)
		}
	}()

	p := tea.NewProgram(
		ui.New(cfg, fleet, poller),
		tea.WithAltScreen(),
	)
	_, err = p.Run()
	return err
}
