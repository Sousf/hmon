package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const oneTemplate = `
hosts: [a]
templates:
  - name: nixos
    image: images:nixos/25.05/cloud
    type: vm
    cpu: 4
    memory: 4GiB
    provision: ./provision/nixos.sh
`

func TestTemplateParses(t *testing.T) {
	c, err := Parse([]byte(oneTemplate))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Templates) != 1 {
		t.Fatalf("got %d templates, want 1", len(c.Templates))
	}
	tpl := c.Templates[0]
	if tpl.Name != "nixos" || tpl.Image != "images:nixos/25.05/cloud" {
		t.Errorf("template = %+v", tpl)
	}
	if !tpl.IsVM() || tpl.CPU != 4 || tpl.Memory != "4GiB" {
		t.Errorf("shape = %+v", tpl)
	}
}

// Omitting the type gives a container, which is what `lxc launch` does on its
// own — a default that silently produced a VM would be a costly surprise.
func TestTemplateTypeDefaultsToContainer(t *testing.T) {
	c, err := Parse([]byte("hosts: [a]\ntemplates:\n  - name: web\n    image: ubuntu:24.04\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Templates[0]; got.Type != TypeContainer || got.IsVM() {
		t.Errorf("type = %q, IsVM = %v", got.Type, got.IsVM())
	}
}

func TestTemplateValidation(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"no name", "hosts: [a]\ntemplates:\n  - image: ubuntu:24.04\n", "no `name:`"},
		{"no image", "hosts: [a]\ntemplates:\n  - name: web\n", "no `image:`"},
		{"bad type", "hosts: [a]\ntemplates:\n  - {name: web, image: u, type: lxc}\n", "want \"container\" or \"vm\""},
		{"negative cpu", "hosts: [a]\ntemplates:\n  - {name: web, image: u, cpu: -1}\n", "negative cpu"},
		{
			"duplicate",
			"hosts: [a]\ntemplates:\n  - {name: web, image: u}\n  - {name: web, image: v}\n",
			"duplicate template name",
		},
		{
			// The likeliest typo, and silently ignoring it would give an
			// instance with none of the limits you asked for.
			"unknown key",
			"hosts: [a]\ntemplates:\n  - {name: web, image: u, mem: 4GiB}\n",
			"field mem not found",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.yaml))
			if err == nil {
				t.Fatalf("accepted %s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// Relative to the config, not to the working directory: the config is what
// names the script, and hmon is as often started from somewhere else.
func TestProvisionResolvesAgainstTheConfigFile(t *testing.T) {
	c, err := Parse([]byte(oneTemplate))
	if err != nil {
		t.Fatal(err)
	}
	c.Path = filepath.Join("/home", "pb", ".config", "hmon", "config.yaml")

	want := filepath.Join("/home", "pb", ".config", "hmon", "provision", "nixos.sh")
	if got := c.ProvisionFor(c.Templates[0]); got != want {
		t.Errorf("ProvisionFor = %q, want %q", got, want)
	}

	// An absolute path is already an answer.
	abs := Template{Provision: filepath.Join("/etc", "hmon", "p.sh")}
	if got := c.ProvisionFor(abs); got != abs.Provision {
		t.Errorf("absolute path was rewritten to %q", got)
	}

	if got := c.ProvisionFor(Template{}); got != "" {
		t.Errorf("a template with no script resolved to %q", got)
	}
}

func TestLaunchTimeoutDefaultsAndValidates(t *testing.T) {
	c, err := Parse([]byte("hosts: [a]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.LaunchTimeout != DefaultLaunchTimeout {
		t.Errorf("LaunchTimeout = %s, want %s", c.LaunchTimeout, DefaultLaunchTimeout)
	}

	c, err = Parse([]byte("hosts: [a]\nlaunch_timeout: 30m\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.LaunchTimeout != 30*time.Minute {
		t.Errorf("LaunchTimeout = %s, want 30m", c.LaunchTimeout)
	}

	if _, err := Parse([]byte("hosts: [a]\nlaunch_timeout: -1s\n")); err == nil {
		t.Error("a negative launch_timeout was accepted")
	}
}

// No templates is a perfectly good config — it just means n has nothing to
// offer — and must not be treated as an error.
func TestNoTemplatesIsValid(t *testing.T) {
	c, err := Parse([]byte("hosts: [a]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Templates) != 0 {
		t.Errorf("got %d templates, want none", len(c.Templates))
	}
}
