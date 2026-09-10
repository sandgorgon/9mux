# Changelog

All notable changes to this project are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project intends to follow [Semantic Versioning](https://semver.org/)
once a first tagged release is cut.

## [Unreleased]

## [0.1.3] - 2026-09-09

### Changed

- Bump `sandgorgon/9p` from v0.7.1 to v0.9.1, and drop the stale
  `// indirect` marker on it in `go.mod` (`mux/browse.go` has always
  imported it directly). No API breakage — both intervening releases
  are additive/opt-in (`client.File.Rename`/`Remove`; optional
  9P2000.u symlink support via `client.WithUnixExtensions()`, which
  9mux doesn't call, so wire behavior toward any server is unchanged).
  Worth taking regardless: v0.8.0 fixed a real path-confinement gap in
  `examples/dirfs` (a symlink at an intermediate path component could
  escape the exported root at syscall time) — 9sh's own `/local`/
  `/env`/`/config`/`/session` namespace binds that 9mux's browsing
  pane points at go through that exact backend. Matches 9sh's own
  bump to v0.9.1 in v0.4.25.
- `sandgorgon/tui` checked against upstream — already at the latest
  tag (`v0.6.2`, matching go.mod); no bump needed.

## [0.1.2] - 2026-09-09

### Changed

- Bump `sandgorgon/tui` to v0.6.2: retunes the Default theme's
  Muted/Border/Success/Warning/Error colors for WCAG contrast,
  colorblind separability, and distinct ANSI-16 fallback slots.

## [0.1.1] - 2026-09-08

### Added

- Session-history table: a directory of 9sh's day-sharded
  `YYYY-MM-DD.nrl` history-log files (as bound at `/session`) renders
  as an aggregated, newest-first table instead of a raw file listing —
  the same structural detection the job table already used, extended
  to a third shape.

## [0.1.0] - 2026-09-07

### Added

- Generic split-tree multiplexer: split/resize/minimize/zoom/close,
  F1-F9 focus-jump, a configurable control strip, real pty hosting via
  `widget.Terminal`, extracted and generalized from 9sh's own `pane`
  package (see README's "Lineage").
- Config-driven command presets (`~/.config/9mux/config`, `name =
  argv...`), with `$SHELL` expansion and a zero-config default.
- The 9P-browsing pane: a native (non-pty) pane kind that points a
  `github.com/sandgorgon/9p` client connection at a 9P root (typically
  a running 9sh's `-listen-unix` socket), rendering a plain directory
  listing or, when a directory's shape matches 9sh's job-control
  protocol, a job table.
- Browsing presets (`name = browse unix:<path>` / `name = browse
  tcp:<host:port>`) alongside command presets.
- Job table: `k` writes a kill command to the selected job's `ctl`
  file, and every non-terminal job gets a wait-driven auto-refresh (no
  polling) that re-lists the table the instant that job finishes.
