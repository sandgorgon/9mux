package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadParsesBrowsePreset(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "9mux")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config")
	contents := "shell = /bin/sh\njobs = browse unix:/tmp/9sh.sock\npeer = browse tcp:localhost:5640\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	presets, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 3 {
		t.Fatalf("got %d presets, want 3: %+v", len(presets), presets)
	}

	shell := presets[0]
	if shell.Name != "shell" || shell.Browse != nil || len(shell.Argv) == 0 {
		t.Errorf("shell preset: got %+v, want a plain command preset", shell)
	}

	jobs := presets[1]
	if jobs.Name != "jobs" || jobs.Browse == nil {
		t.Fatalf("jobs preset: got %+v, want a browse preset", jobs)
	}
	if jobs.Browse.Network != "unix" || jobs.Browse.Addr != "/tmp/9sh.sock" {
		t.Errorf("jobs preset target: got %+v, want {unix /tmp/9sh.sock}", jobs.Browse)
	}
	if jobs.Argv != nil {
		t.Errorf("jobs preset: got Argv %v, want nil", jobs.Argv)
	}

	peer := presets[2]
	if peer.Name != "peer" || peer.Browse == nil {
		t.Fatalf("peer preset: got %+v, want a browse preset", peer)
	}
	if peer.Browse.Network != "tcp" || peer.Browse.Addr != "localhost:5640" {
		t.Errorf("peer preset target: got %+v, want {tcp localhost:5640}", peer.Browse)
	}
}

// TestLoadAttachesBrowseCompanionToPrecedingPreset confirms a
// "<name>.browse = ..." line right after a command preset named
// <name> attaches a BrowseCompanion to that preset instead of
// appending a new one.
func TestLoadAttachesBrowseCompanionToPrecedingPreset(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "9mux")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config")
	contents := "kyu = 9sh --listen-unix /tmp/9sh-$MUX_PID-{id}.sock\n" +
		"kyu.browse = unix:/tmp/9sh-$MUX_PID-{id}.sock\n" +
		"shell = /bin/sh\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	presets, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 2 {
		t.Fatalf("got %d presets, want 2 (the .browse line shouldn't add one): %+v", len(presets), presets)
	}

	kyu := presets[0]
	if kyu.Name != "kyu" || kyu.BrowseCompanion == nil {
		t.Fatalf("kyu preset: got %+v, want a BrowseCompanion attached", kyu)
	}
	want := "/tmp/9sh-$MUX_PID-{id}.sock"
	if kyu.BrowseCompanion.Network != "unix" || kyu.BrowseCompanion.Addr != want {
		t.Errorf("kyu.BrowseCompanion: got %+v, want {unix %s}", kyu.BrowseCompanion, want)
	}

	shell := presets[1]
	if shell.Name != "shell" || shell.BrowseCompanion != nil {
		t.Errorf("shell preset: got %+v, want no BrowseCompanion", shell)
	}
}

// TestLoadSkipsBrowseCompanionForUnknownBase confirms a "<name>.browse
// = ..." line is silently dropped (not turned into its own preset)
// when <name> hasn't already been parsed as a command preset — same
// lenient-skip discipline as a malformed browse target.
func TestLoadSkipsBrowseCompanionForUnknownBase(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "9mux")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config")
	// ghost.browse appears with no preceding "ghost = ..." line.
	contents := "ghost.browse = unix:/tmp/ghost.sock\nshell = /bin/sh\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	presets, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 1 || presets[0].Name != "shell" {
		t.Fatalf("got %+v, want only the shell preset to survive", presets)
	}
}

func TestLoadSkipsMalformedBrowseTarget(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "9mux")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config")
	// "browse" followed by a target with no recognized unix:/tcp: prefix
	// is silently skipped rather than producing a broken preset — same
	// "just skip an invalid line" discipline Load already applies to a
	// malformed command preset.
	contents := "bad = browse nope:whatever\nshell = /bin/sh\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	presets, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 1 || presets[0].Name != "shell" {
		t.Fatalf("got %+v, want only the shell preset to survive", presets)
	}
}

// TestLoadBareBrowseFallsThroughAsCommand documents a deliberate edge
// case: "name = browse" with no target at all doesn't hit the "browse "
// (with trailing space) prefix check, since value is already
// whitespace-trimmed by the time that check runs — so it falls through
// and becomes an ordinary command preset that runs a literal program
// named "browse". Genuinely ambiguous with a real command by that
// name; Load doesn't try to disambiguate it.
func TestLoadBareBrowseFallsThroughAsCommand(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "9mux")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("bad = browse\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	presets, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(presets) != 1 || presets[0].Browse != nil || len(presets[0].Argv) != 1 || presets[0].Argv[0] != "browse" {
		t.Fatalf("got %+v, want a single command preset running literal \"browse\"", presets)
	}
}

func TestParseBrowseTarget(t *testing.T) {
	cases := []struct {
		in      string
		wantNet string
		wantErr bool
	}{
		{"unix:/tmp/9sh.sock", "unix", false},
		{"tcp:localhost:5640", "tcp", false},
		{"unix:", "", true},
		{"tcp:", "", true},
		{"nope:whatever", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := parseBrowseTarget(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseBrowseTarget(%q): got nil error, want one", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBrowseTarget(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got.Network != c.wantNet {
			t.Errorf("parseBrowseTarget(%q): got network %q, want %q", c.in, got.Network, c.wantNet)
		}
	}
}
