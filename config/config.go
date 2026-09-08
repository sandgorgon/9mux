// Package config loads 9mux's settings: the named command presets its
// control strip and title-bar split flow offer (see mux.Preset). Kept
// deliberately dumb — plain "name = argv..." lines, no dependency on
// any particular shell or tool — since 9mux itself has no notion of
// what a preset actually is beyond "a command to run in a pty," plus
// one narrow second preset shape (see BrowseTarget) for the 9P-
// browsing pane.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Preset is one named command the control strip's "+" buttons and the
// title-bar split flow (digit keys, in config order) can launch as a
// new pane. Exactly one of Argv or Browse is set — a command preset
// (the common case) or a 9P-browsing preset (see BrowseTarget).
type Preset struct {
	Name   string
	Argv   []string
	Browse *BrowseTarget
}

// BrowseTarget is a preset that opens a 9P-browsing pane instead of a
// pty-hosted command: Network is "unix" or "tcp" (net.Dial's own
// network argument), Addr the corresponding path or host:port. Written
// in config as `name = browse unix:/path/to/socket` or
// `name = browse tcp:host:port` — the same two address shapes 9sh's
// own remote.Dial dispatches on, reimplemented narrowly here rather
// than imported, since 9mux never depends on 9sh (see README's "Where
// this is headed").
type BrowseTarget struct {
	Network string
	Addr    string
}

// defaultConfig seeds a fresh install with just $SHELL — enough to be
// immediately useful with zero configuration; anything else (a "kyu"
// preset running 9sh, a "top" preset running htop, ...) is something
// the user adds themselves.
const defaultConfig = "shell = $SHELL\n"

// Dir returns 9mux's settings directory, ~/.config/9mux.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "9mux"), nil
}

func file() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config"), nil
}

// EnsureDefault creates Dir() and seeds it with defaultConfig if the
// config file doesn't already exist. Never overwrites an existing one.
func EnsureDefault() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path, err := file()
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfig), 0644)
}

// Load reads presets from the config file, one "name = argv..." per
// line (blank lines and "#"-prefixed comments skipped). A bare $SHELL
// token in a value expands to the real $SHELL env var (falling back to
// /bin/sh if unset) — the one piece of built-in expansion, since it's
// the only value that can't be known until run time. A missing file,
// or one with no valid presets, falls back to defaultPresets() rather
// than leaving 9mux with nothing to offer.
func Load() ([]Preset, error) {
	path, err := file()
	if err != nil {
		return defaultPresets(), nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultPresets(), nil
		}
		return nil, err
	}
	defer f.Close()

	var presets []Preset
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if rest, ok := strings.CutPrefix(value, "browse "); ok {
			target, err := parseBrowseTarget(strings.TrimSpace(rest))
			if err != nil {
				continue
			}
			presets = append(presets, Preset{Name: strings.TrimSpace(name), Browse: target})
			continue
		}
		argv := expandArgv(value)
		if len(argv) == 0 {
			continue
		}
		presets = append(presets, Preset{Name: strings.TrimSpace(name), Argv: argv})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(presets) == 0 {
		return defaultPresets(), nil
	}
	return presets, nil
}

// parseBrowseTarget parses the address half of a "browse <target>"
// config value: "unix:<path>" or "tcp:<host:port>".
func parseBrowseTarget(s string) (*BrowseTarget, error) {
	if rest, ok := strings.CutPrefix(s, "unix:"); ok && rest != "" {
		return &BrowseTarget{Network: "unix", Addr: rest}, nil
	}
	if rest, ok := strings.CutPrefix(s, "tcp:"); ok && rest != "" {
		return &BrowseTarget{Network: "tcp", Addr: rest}, nil
	}
	return nil, fmt.Errorf("config: browse target %q must start with unix: or tcp:", s)
}

func expandArgv(value string) []string {
	fields := strings.Fields(value)
	for i, f := range fields {
		if f == "$SHELL" {
			if sh := os.Getenv("SHELL"); sh != "" {
				fields[i] = sh
			} else {
				fields[i] = "/bin/sh"
			}
		}
	}
	return fields
}

func defaultPresets() []Preset {
	return []Preset{{Name: "shell", Argv: expandArgv("$SHELL")}}
}
