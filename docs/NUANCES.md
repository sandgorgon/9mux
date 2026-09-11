# Nuances, caveats, and FAQ

Details that matter once you're actually using a 9P-browsing pane, but
that would clutter the README's basic usage instructions. See
[../README.md](../README.md) for the pane's basic key bindings and
[CONFIGURATION.md](CONFIGURATION.md) for how to point a browsing pane
at something.

## Why doesn't a listing refresh on its own?

By design. Inside a browsing pane, real I/O only ever happens on
explicit actions (Enter, Backspace, `r`) plus one deliberate exception
(job auto-refresh, below) — never a background poll. This matches
9sh's own now-removed job/namespace viewer panes, which made the same
"a quick glance, not a live dashboard" choice on purpose.

## How does 9mux know a directory is a job table, not a plain listing?

A directory is rendered as a **job table** whenever its shape
structurally matches 9sh's job-control protocol (`job/fs.go`: a
`clone` file plus one numerically-named directory per job) — not by
matching a hardcoded `/jobs` path. So this works against any 9P server
shaped that way, not just 9sh.

In a job table, `k` writes a kill command to the selected job's `ctl`
file — the one write affordance this pane offers; anything more
(stop/resume, editing `argv`/`env`/`cwd` in place) is still open.

## Why did my job table update itself without me pressing `r`?

**Job auto-refresh.** Every non-terminal job (state other than
`done`/`failed`/`killed`) visible in a job table gets a blocking read
on its own `wait` file (`job/fs.go`'s `waitFile` — resolves the
instant that job reaches a terminal state, per `job.WaitFor`), which
on return triggers exactly one fresh listing of that directory — so a
job table updates itself the moment a watched job finishes, no manual
`r` needed for that one, most common event ("is my job done yet").

This is a deliberate, narrow exception to the "no background poll"
rule above: one blocking wait per non-terminal job, deduplicated
(re-listing the same directory — a manual `r`, or another job's own
auto-refresh — never starts a second watch for a job already being
watched), reset when the pane navigates to a different directory. Full
output tailing (`stdout`/`stderr`/`events`) is a different, bigger
feature than this and isn't built (see the README's Status section).

### Known limitation: watches can't be canceled individually

`p9/client`'s `File.Read` takes no caller-supplied context, so an
in-flight wait watch can't be canceled individually — navigating away
from a directory before a watched job finishes leaves that watch (one
open fid, one blocked goroutine) running until either the job actually
finishes or the pane's connection closes entirely (which does unblock
every outstanding watch at once, via the same `io.Closer` mechanism
`widget.Terminal` uses for its pty). Bounded — each watch resolves on
its own eventually — but real; worth knowing before watching many jobs
across many directory visits in one long-lived pane.

## What's the history table, and why is it read-only?

**Session-history table.** A directory whose entries are all regular
files matching 9sh's day-sharded history-log naming (`session/
session.go`'s `dayShard`: `YYYY-MM-DD.nrl`) renders as an aggregated,
newest-first history table (timestamp, host, exit/signal, argv)
instead of a raw file listing — the same structural, not
path-name-based, detection the job table uses. This is what lets the
browsing pane reach 9sh's `/session` (bound via plain `dirfs` over the
real on-disk session repo, since 9sh's own commit `cc1302a`) usefully,
rather than just showing raw JSON-lines file content in the file
preview.

Capped at 200 records and, since an append-only log has no single
"resolves once" file the way a job's `wait` does, plain
refresh-on-demand (`r`) like everything but the job table — no
auto-refresh, no drill-down (same as the job table), no write
affordance (this is a read-only history, not a live resource).
