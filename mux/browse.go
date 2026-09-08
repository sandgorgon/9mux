// Browsing pane: 9mux's one native (non-pty) pane kind, decided in
// the project's own design notes ("Option 4" — see README's "Where
// this is headed") over three rejected alternatives (a pty-hosted
// mini-view subcommand, a generic plugin protocol, importing 9sh's Go
// packages directly). It points a p9/client connection at a 9P root
// (typically a running 9sh's -listen-unix socket) and renders three
// well-known shapes: a plain directory listing; when a directory's
// shape matches 9sh's job-control protocol (job/fs.go), a job table
// with a 'k' keybinding to write a kill command to the selected job's
// ctl file; and, when a directory's entries all match 9sh's day-sharded
// session-history log naming (session/session.go's dayShard,
// YYYY-MM-DD.nrl), a session-history table aggregated across those
// files. Every non-terminal job in a job table also gets a blocking
// wait watch (see waitJobCmd/startJobWaitWatches) that triggers one
// fresh listing the instant that job finishes — the one deliberate
// exception to this pane's otherwise strict refresh-on-demand
// discipline (see README's "Job auto-refresh"); the session-history
// table has no equivalent (an append-only log has no single "resolves
// once" file to watch), so it stays plain refresh-on-demand like
// everything else.
//
// This is the one deliberate exception to "every pane is Kind-free"
// (see model.go's own package doc comment): Spec/paneState carry
// exactly one more variant (Browse/browse), nothing more general.
package mux

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	p9 "github.com/sandgorgon/9p"
	"github.com/sandgorgon/9p/client"
	"github.com/sandgorgon/tui/cell"
	"github.com/sandgorgon/tui/input"
	"github.com/sandgorgon/tui/layout"
	"github.com/sandgorgon/tui/style"
	"github.com/sandgorgon/tui/tui"
	"github.com/sandgorgon/tui/widget"
)

// BrowseSpec describes a 9P-browsing pane to create — see Spec's own
// doc comment for how it relates to Command.
type BrowseSpec struct {
	Network string // "unix" or "tcp", net.Dial's own network argument
	Addr    string
}

// browseState is a browsing pane's business state — paneState.browse,
// nil for an ordinary Terminal pane. Exactly one of entries/jobRows is
// populated at a time, chosen by listBrowseCmd's structural detection
// (see jobDirIDs); previewPath additionally set switches the pane into
// a read-only file-content view instead of either listing.
type browseState struct {
	network, addr string

	client  *client.Client
	connErr string

	path []string // path elements from the attached root; nil = root

	// directory-listing mode.
	entries []p9.Stat
	listErr string

	// job-table mode (see jobDirIDs): populated instead of entries when
	// the current directory's shape matches 9sh's job-control protocol.
	// jobIDs[i] is jobRows[i]'s numeric directory name, threaded
	// through so a 'k' kill action knows which job the selected row
	// refers to without re-parsing the formatted row text. jobTerminal[i]
	// is whether that job had already reached a terminal state as of
	// this listing — see watchingJobs below.
	jobRows     []string
	jobIDs      []int
	jobTerminal []bool

	// session-history mode (see sessionHistoryFileNames): populated
	// instead of entries/jobRows when the current directory's entries
	// all match 9sh's day-sharded session-history log naming. Rows are
	// aggregated across every matching file, newest first, capped at
	// sessionHistoryLimit — see loadSessionRows.
	sessionRows []string

	// watchingJobs is the set of job ids this pane currently has an
	// outstanding waitJobCmd blocked on (see that function) — job ids,
	// not row indices, so it stays meaningful across a re-listing that
	// reorders or drops rows. Reset to nil whenever the browsed path
	// changes (see the browseListedMsg handler): watches are scoped to
	// "this pane, sitting on this one directory," not the pane's whole
	// lifetime — see waitJobCmd's own doc comment for why a watch begun
	// before navigating away can still outlive that reset.
	watchingJobs map[int]bool

	cursor int

	// file-preview mode: set instead of showing entries/jobRows once a
	// file (not directory) entry is opened via Enter.
	previewPath  string
	previewLines []string
	previewErr   string

	// killMsg is a transient one-line status shown in the pane's title
	// bar (see paneNode) after a 'k' kill action; cleared on the next
	// successful listing.
	killMsg string
}

// connectTimeout bounds how long a browsing pane's initial dial can
// take — a hung/unreachable socket shouldn't leave the pane stuck in
// "connecting" forever with no feedback.
const connectTimeout = 5 * time.Second

// connectCmdForPane returns the Cmd that dials p's target if p is a
// browsing pane, or nil otherwise (safe to Batch unconditionally
// alongside other pane-creation Cmds — see Init/Update's splitPaneMsg/
// addPaneMsg cases).
func connectCmdForPane(p *paneState) tui.Cmd {
	if p == nil || p.browse == nil {
		return nil
	}
	return connectBrowseCmd(p.id, p.browse.network, p.browse.addr)
}

func connectBrowseCmd(id int, network, addr string) tui.Cmd {
	return func() tui.Msg {
		conn, err := net.DialTimeout(network, addr, connectTimeout)
		if err != nil {
			return browseConnectedMsg{id: id, err: err}
		}
		c, err := client.NewClient(conn)
		if err != nil {
			conn.Close()
			return browseConnectedMsg{id: id, err: err}
		}
		if _, err := c.Attach("9mux", ""); err != nil {
			c.Close()
			return browseConnectedMsg{id: id, err: err}
		}
		return browseConnectedMsg{id: id, client: c}
	}
}

// listBrowseCmd lists path and classifies its shape — a job table (see
// jobDirIDs) or a plain listing — never directly in Update, per the
// "real I/O belongs in a Cmd" discipline this pane kind's reference
// implementation (9sh's own jobviewer.go/browser.go, at the commit
// this project's README's "Lineage" section names) already established.
func listBrowseCmd(id int, c *client.Client, path []string) tui.Cmd {
	return func() tui.Msg {
		f, err := c.Open(joinPath(path), p9.OREAD)
		if err != nil {
			return browseListedMsg{id: id, path: path, err: err}
		}
		defer f.Close()
		stats, err := f.ReadDir()
		if err != nil {
			return browseListedMsg{id: id, path: path, err: err}
		}
		sort.Slice(stats, func(i, j int) bool { return stats[i].Name < stats[j].Name })

		if ids, ok := jobDirIDs(stats); ok {
			rows, rowIDs, terminal := loadJobRows(c, path, ids)
			// Non-nil even for zero jobs: browseListNode/browseRowCount
			// key job-table mode off jobRows != nil, and an empty jobs
			// directory should still render (and count rows) as an empty
			// job table, not silently fall back to plain-listing mode.
			if rows == nil {
				rows = []string{}
			}
			if rowIDs == nil {
				rowIDs = []int{}
			}
			if terminal == nil {
				terminal = []bool{}
			}
			return browseListedMsg{id: id, path: path, jobRows: rows, jobIDs: rowIDs, jobTerminal: terminal}
		}
		if names, ok := sessionHistoryFileNames(stats); ok {
			rows, err := loadSessionRows(c, path, names, sessionHistoryLimit)
			if err != nil {
				return browseListedMsg{id: id, path: path, err: err}
			}
			if rows == nil {
				rows = []string{}
			}
			return browseListedMsg{id: id, path: path, sessionRows: rows}
		}
		return browseListedMsg{id: id, path: path, entries: stats}
	}
}

// jobDirIDs reports whether stats' shape matches 9sh's job-control
// protocol (job/fs.go: one write-only "clone" file plus one
// numerically-named directory per job) — detected structurally, not by
// matching a hardcoded "/jobs" path, so any 9P root shaped like this
// renders as a job table, not just one served by 9sh specifically. An
// empty jobs directory (no jobs started yet) still matches: "clone"
// present, zero id directories, nothing else.
func jobDirIDs(stats []p9.Stat) (ids []int, ok bool) {
	hasClone := false
	for _, s := range stats {
		switch {
		case s.Name == "clone" && !s.Qid.IsDir():
			hasClone = true
		case s.Qid.IsDir():
			id, err := strconv.Atoi(s.Name)
			if err != nil {
				return nil, false
			}
			ids = append(ids, id)
		default:
			return nil, false
		}
	}
	if !hasClone {
		return nil, false
	}
	sort.Ints(ids)
	return ids, true
}

// jobStatus mirrors the JSON shape 9sh's job.Status writes to each
// job's status file (job/fs.go's statusFile.Read) — a data-shape
// dependency only, not a Go package import, so this pane kind stays
// independent of 9sh per the README's "never a dependency on 9sh
// itself."
type jobStatus struct {
	ID       int      `json:"id"`
	Kind     string   `json:"kind"`
	State    string   `json:"state"`
	Argv     []string `json:"argv,omitempty"`
	Pid      int      `json:"pid,omitempty"`
	ExitCode *int     `json:"exit_code,omitempty"`
	Signal   string   `json:"signal,omitempty"`
	Err      string   `json:"error,omitempty"`
}

// jobTerminalStates mirrors job.State.Terminal() (9sh's job/job.go) —
// the same data-shape-only dependency jobStatus itself already is, not
// a Go package import. Used to decide which jobs are worth a
// waitJobCmd (a job already done/failed/killed will never resolve one
// again).
var jobTerminalStates = map[string]bool{"done": true, "failed": true, "killed": true}

func (st jobStatus) terminal() bool { return jobTerminalStates[st.State] }

// loadJobRows fetches and formats one row per job id under path. A
// per-job read failure becomes that job's own row text rather than
// failing the whole listing — one bad job shouldn't hide every other
// one; a job whose status couldn't be read counts as terminal (no
// point starting a wait watch on it).
func loadJobRows(c *client.Client, path []string, ids []int) (rows []string, rowIDs []int, terminal []bool) {
	for _, id := range ids {
		st, err := readJobStatus(c, path, id)
		row, term := formatJobRow(st), st.terminal()
		if err != nil {
			row, term = formatJobRowFailed(id, err.Error()), true
		}
		rows = append(rows, row)
		rowIDs = append(rowIDs, id)
		terminal = append(terminal, term)
	}
	return rows, rowIDs, terminal
}

func readJobStatus(c *client.Client, path []string, id int) (jobStatus, error) {
	statusPath := joinPath(append(append([]string{}, path...), strconv.Itoa(id), "status"))
	f, err := c.Open(statusPath, p9.OREAD)
	if err != nil {
		return jobStatus{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return jobStatus{}, err
	}
	var st jobStatus
	if err := json.Unmarshal(b, &st); err != nil {
		return jobStatus{}, err
	}
	return st, nil
}

func formatJobRow(st jobStatus) string {
	argv := strings.Join(st.Argv, " ")
	if argv == "" {
		argv = "(none)"
	}
	status := st.State
	switch {
	case st.Err != "":
		status = "error: " + st.Err
	case st.Signal != "":
		status += " (sig:" + st.Signal + ")"
	case st.ExitCode != nil:
		status += fmt.Sprintf(" (exit:%d)", *st.ExitCode)
	}
	return fmt.Sprintf("%-4d %-10s %-20s %s", st.ID, st.Kind, status, argv)
}

func formatJobRowFailed(id int, errText string) string {
	return fmt.Sprintf("%-4d error: %s", id, errText)
}

// sessionHistoryPattern matches 9sh's day-sharded session-history log
// filenames (session/session.go's dayShard: t.Format("2006-01-02") +
// ".nrl") — the structural signal sessionHistoryFileNames keys off of,
// not a hardcoded "/session" path, matching jobDirIDs' own "shape, not
// name" discipline.
var sessionHistoryPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}\.nrl$`)

// sessionHistoryFileNames reports whether stats' shape matches 9sh's
// session-history log directory: one or more regular files, every one
// matching sessionHistoryPattern, nothing else. Returns their names
// sorted descending — newest day first, the same filename order
// loadSessionRows' early-stop walk relies on. An empty directory
// doesn't match (ok=false): unlike jobDirIDs' "clone" marker file,
// there's no anchor to distinguish "history, no records logged yet"
// from "any other empty directory" — falling back to a plain listing
// showing "(empty)" is a perfectly fine answer for that case anyway.
func sessionHistoryFileNames(stats []p9.Stat) (names []string, ok bool) {
	if len(stats) == 0 {
		return nil, false
	}
	for _, s := range stats {
		if s.Qid.IsDir() || !sessionHistoryPattern.MatchString(s.Name) {
			return nil, false
		}
		names = append(names, s.Name)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	return names, true
}

// sessionRecord mirrors the JSON shape 9sh's session.Record writes to
// each line of a day-sharded history file (session/session.go) — the
// same data-shape-only dependency jobStatus itself already is, not a
// Go package import.
type sessionRecord struct {
	TSStart     time.Time `json:"ts_start"`
	TSEnd       time.Time `json:"ts_end"`
	Host        string    `json:"host"`
	JobID       int       `json:"job_id"`
	Cwd         string    `json:"cwd,omitempty"`
	Argv        []string  `json:"argv,omitempty"`
	Exit        *int      `json:"exit,omitempty"`
	Signal      string    `json:"signal,omitempty"`
	Kind        string    `json:"kind"`
	Detached    bool      `json:"detached,omitempty"`
	RemoteHost  string    `json:"remote_host,omitempty"`
	RemoteJobID int       `json:"remote_job_id,omitempty"`
}

// sessionHistoryLimit caps how many history records loadSessionRows
// keeps — a glance-able recent-activity view, not a full-history
// browser, matching 9sh's own removed sessionviewer.go's
// sessionViewerLimit (200) exactly.
const sessionHistoryLimit = 200

// loadSessionRows aggregates history across names (already sorted
// newest-file-first — see sessionHistoryFileNames), formatting up to
// limit rows, newest first. Mirrors session.ReadRecent's own early-stop
// algorithm (9sh's session/read.go), ported from local disk reads to
// p9/client reads: a day's file is itself oldest-first (append-only),
// so each file's own records are walked backwards, and the walk over
// files stops the moment limit rows have been gathered rather than
// reading every historical file just to show the most recent few.
func loadSessionRows(c *client.Client, path []string, names []string, limit int) ([]string, error) {
	var rows []string
	for _, name := range names {
		recs, err := readSessionFile(c, path, name)
		if err != nil {
			return rows, err
		}
		for i := len(recs) - 1; i >= 0; i-- {
			rows = append(rows, formatSessionRow(recs[i]))
			if len(rows) >= limit {
				return rows, nil
			}
		}
	}
	return rows, nil
}

// readSessionFile reads and parses one day-sharded history file's
// records, oldest-first (append-only, matching on-disk order). A
// malformed line is skipped rather than failing the whole file — one
// bad record shouldn't hide every other one in that day.
func readSessionFile(c *client.Client, path []string, name string) ([]sessionRecord, error) {
	filePath := joinPath(append(append([]string{}, path...), name))
	f, err := c.Open(filePath, p9.OREAD)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var recs []sessionRecord
	for line := range strings.SplitSeq(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec sessionRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

func formatSessionRow(rec sessionRecord) string {
	ts := rec.TSEnd
	if ts.IsZero() {
		ts = rec.TSStart
	}
	when := "?"
	if !ts.IsZero() {
		when = ts.Local().Format("2006-01-02 15:04:05")
	}
	status := "?"
	switch {
	case rec.Signal != "":
		status = "sig:" + rec.Signal
	case rec.Exit != nil:
		status = fmt.Sprintf("exit:%d", *rec.Exit)
	}
	argv := strings.Join(rec.Argv, " ")
	if argv == "" {
		argv = "(" + rec.Kind + ")"
	}
	return fmt.Sprintf("%s  %-10s %-10s %s", when, rec.Host, status, argv)
}

// previewMaxBytes caps how much of a file loadPreviewCmd reads — a
// glance-able file preview (status/argv/env/cwd — small config/state
// files), not a full pager for arbitrarily large content.
const previewMaxBytes = 1 << 20 // 1MiB

func loadPreviewCmd(id int, c *client.Client, path []string) tui.Cmd {
	return func() tui.Msg {
		p := joinPath(path)
		f, err := c.Open(p, p9.OREAD)
		if err != nil {
			return browsePreviewLoadedMsg{id: id, path: p, err: err}
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, previewMaxBytes))
		if err != nil {
			return browsePreviewLoadedMsg{id: id, path: p, err: err}
		}
		text := strings.TrimRight(string(b), "\n")
		var lines []string
		if text != "" {
			lines = strings.Split(text, "\n")
		}
		return browsePreviewLoadedMsg{id: id, path: p, lines: lines}
	}
}

// killJobCmd writes "kill" to job jobID's ctl file (job/fs.go's
// ctlFile, write-only, taking a trimmed command string per job.Ctl) —
// the write-side affordance the README's "Where this is headed"
// section left as an open question, resolved here as the minimum
// useful case rather than a general "write to this 9P file" UI.
func killJobCmd(id int, c *client.Client, path []string, jobID int) tui.Cmd {
	return func() tui.Msg {
		ctlPath := joinPath(append(append([]string{}, path...), strconv.Itoa(jobID), "ctl"))
		f, err := c.Open(ctlPath, p9.OWRITE)
		if err != nil {
			return browseKillResultMsg{id: id, err: err}
		}
		defer f.Close()
		_, err = f.Write([]byte("kill"))
		return browseKillResultMsg{id: id, err: err}
	}
}

// waitJobCmd blocks on job jobID's wait file (job/fs.go's waitFile —
// Read blocks until the job reaches a terminal state, per job.
// WaitFor) and, on return, is the trigger for a fresh listBrowseCmd
// (see handleBrowseMsg's browseJobWaitMsg case) — the wait-driven
// auto-refresh: a job table stops needing a manual 'r' to notice a
// watched job finished. The response content itself is unused; the
// read returning at all (successfully or with an error, e.g. the
// pane's connection closing) is the whole signal.
//
// Known limitation: p9/client's File.Read has no caller-supplied
// context, so this blocked read can't be canceled short of closing the
// whole connection (which browseConnNode already does when the pane
// itself closes — see its own doc comment — promptly unblocking this
// with an error). Navigating away from this directory without closing
// the pane does *not* cancel an in-flight watch: it keeps blocking,
// server-side fid and all, until the job actually finishes. Bounded
// (one per job ever watched, each resolving on its own eventually),
// not unbounded, but real — worth knowing before watching many jobs
// across many directory visits in one long-lived pane.
func waitJobCmd(id int, c *client.Client, path []string, jobID int) tui.Cmd {
	return func() tui.Msg {
		waitPath := joinPath(append(append([]string{}, path...), strconv.Itoa(jobID), "wait"))
		f, err := c.Open(waitPath, p9.OREAD)
		if err != nil {
			return browseJobWaitMsg{id: id, path: path, jobID: jobID, err: err}
		}
		defer f.Close()
		_, err = io.ReadAll(f)
		return browseJobWaitMsg{id: id, path: path, jobID: jobID, err: err}
	}
}

func joinPath(elems []string) string {
	if len(elems) == 0 {
		return "/"
	}
	return "/" + strings.Join(elems, "/")
}

func pathEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// browseRowCount is how many selectable rows browseListNode currently
// renders — used to clamp b.cursor consistently after every state
// change, matching browseListNode's own item-building logic exactly
// (see that function for why jobRows and entries+".." aren't counted
// the same way).
func browseRowCount(b *browseState) int {
	if b.jobRows != nil {
		return len(b.jobRows)
	}
	if b.sessionRows != nil {
		return len(b.sessionRows)
	}
	n := len(b.entries)
	if len(b.path) > 0 {
		n++ // ".." row
	}
	return n
}

// browseSelectedEntryIndex maps b.cursor — as rendered by
// browseListNode, where a ".." row occupies index 0 whenever
// len(b.path) > 0 — to an index into b.entries, or ok=false if the
// cursor doesn't currently land on a real entry (on the ".." row
// itself, callers check that separately; or genuinely out of range).
func browseSelectedEntryIndex(b *browseState) (idx int, ok bool) {
	off := 0
	if len(b.path) > 0 {
		off = 1
	}
	i := b.cursor - off
	if i < 0 || i >= len(b.entries) {
		return 0, false
	}
	return i, true
}

// startJobWaitWatches returns a Cmd (nil if there's nothing to do) that
// starts a waitJobCmd for every job in b's current job-table listing
// that's both non-terminal and not already being watched (see
// b.watchingJobs) — called after every fresh listing (a directory
// entered, a manual 'r', or an earlier watch's own auto-refresh), so a
// job already being watched never gets a second, redundant watch.
func startJobWaitWatches(id int, b *browseState) tui.Cmd {
	if b.client == nil || b.jobIDs == nil {
		return nil
	}
	var cmds []tui.Cmd
	for i, jobID := range b.jobIDs {
		if i < len(b.jobTerminal) && b.jobTerminal[i] {
			continue
		}
		if b.watchingJobs == nil {
			b.watchingJobs = make(map[int]bool)
		}
		if b.watchingJobs[jobID] {
			continue
		}
		b.watchingJobs[jobID] = true
		cmds = append(cmds, waitJobCmd(id, b.client, b.path, jobID))
	}
	return tui.Batch(cmds...)
}

// ---- Msg types ----

type browseConnectedMsg struct {
	id     int
	client *client.Client
	err    error
}
type browseListedMsg struct {
	id          int
	path        []string
	entries     []p9.Stat
	jobRows     []string
	jobIDs      []int
	jobTerminal []bool
	sessionRows []string
	err         error
}
type browseMoveMsg struct{ id, delta int }
type browseClickMsg struct{ id, index int }
type browseEnterMsg struct{ id int }
type browseUpMsg struct{ id int }
type browseRefreshMsg struct{ id int }
type browsePreviewLoadedMsg struct {
	id    int
	path  string
	lines []string
	err   error
}
type browsePreviewCloseMsg struct{ id int }
type browseKillMsg struct{ id int }
type browseKillResultMsg struct {
	id  int
	err error
}

// browseJobWaitMsg is waitJobCmd's result — see that function's doc
// comment. path is the directory the watch was started against, so
// the handler can tell a genuinely stale watch (the pane has since
// navigated elsewhere) from one still worth acting on.
type browseJobWaitMsg struct {
	id    int
	path  []string
	jobID int
	err   error
}

// handleBrowseMsg dispatches every browse-pane Msg type, called from
// Model.Update's default case (see model.go) — kept in this file
// rather than inline in model.go's own switch so browse.go stays the
// single place this pane kind's whole behavior lives. Returns
// handled=false for any Msg it doesn't recognize, so Update's caller
// can fall through unchanged.
func (m Model) handleBrowseMsg(msg tui.Msg) (next Model, cmd tui.Cmd, handled bool) {
	switch mm := msg.(type) {
	case browseConnectedMsg:
		p := m.find(mm.id)
		if p == nil || p.browse == nil {
			if mm.client != nil {
				mm.client.Close() // pane closed before connect finished
			}
			return m, nil, true
		}
		if mm.err != nil {
			p.browse.connErr = mm.err.Error()
			return m, nil, true
		}
		p.browse.client = mm.client
		return m, listBrowseCmd(mm.id, mm.client, p.browse.path), true

	case browseListedMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			b := p.browse
			pathChanged := !pathEqual(b.path, mm.path)
			b.path = mm.path
			b.killMsg = ""
			if mm.err != nil {
				b.listErr = mm.err.Error()
				b.entries, b.jobRows, b.jobIDs, b.jobTerminal, b.sessionRows = nil, nil, nil, nil, nil
			} else {
				b.listErr = ""
				b.entries, b.jobRows, b.jobIDs, b.jobTerminal, b.sessionRows = mm.entries, mm.jobRows, mm.jobIDs, mm.jobTerminal, mm.sessionRows
			}
			b.cursor = clamp(b.cursor, 0, max0(browseRowCount(b)-1))
			if pathChanged {
				// Watches are scoped to "sitting on this one directory" —
				// see watchingJobs' own doc comment for why a watch from
				// before this navigation can still be running regardless.
				b.watchingJobs = nil
			}
			return m, startJobWaitWatches(mm.id, b), true
		}
		return m, nil, true

	case browseJobWaitMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			b := p.browse
			// Resolved (or errored) either way — free to watch this job
			// id again on some later listing, e.g. if it somehow still
			// reports non-terminal.
			delete(b.watchingJobs, mm.jobID)
			if b.client != nil && pathEqual(b.path, mm.path) {
				return m, listBrowseCmd(mm.id, b.client, b.path), true
			}
		}
		return m, nil, true

	case browseMoveMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			if n := browseRowCount(p.browse); n > 0 {
				p.browse.cursor = clamp(p.browse.cursor+mm.delta, 0, n-1)
			}
		}
		return m, nil, true

	case browseClickMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			if n := browseRowCount(p.browse); n > 0 {
				p.browse.cursor = clamp(mm.index, 0, n-1)
			}
		}
		return m, nil, true

	case browseRefreshMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil && p.browse.client != nil {
			return m, listBrowseCmd(mm.id, p.browse.client, p.browse.path), true
		}
		return m, nil, true

	case browseEnterMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			b := p.browse
			switch {
			case b.client == nil, b.previewPath != "", b.jobRows != nil, b.sessionRows != nil:
				// Not connected yet; already previewing (Enter has no
				// further meaning there); or job-table/session-table
				// mode, neither of which has a drill-down yet — see
				// browseState's own doc comment.
			case len(b.path) > 0 && b.cursor == 0:
				// The ".." row itself (see browseListNode) — same as
				// Backspace, not "entries[-1]".
				return m, listBrowseCmd(mm.id, b.client, b.path[:len(b.path)-1]), true
			default:
				if idx, ok := browseSelectedEntryIndex(b); ok {
					entry := b.entries[idx]
					newPath := append(append([]string{}, b.path...), entry.Name)
					if entry.Qid.IsDir() {
						return m, listBrowseCmd(mm.id, b.client, newPath), true
					}
					return m, loadPreviewCmd(mm.id, b.client, newPath), true
				}
			}
		}
		return m, nil, true

	case browseUpMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			b := p.browse
			switch {
			case b.client == nil:
			case b.previewPath != "":
				b.previewPath, b.previewLines, b.previewErr = "", nil, ""
			case len(b.path) > 0:
				return m, listBrowseCmd(mm.id, b.client, b.path[:len(b.path)-1]), true
			}
		}
		return m, nil, true

	case browsePreviewLoadedMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			b := p.browse
			b.previewPath = mm.path
			if mm.err != nil {
				b.previewErr = mm.err.Error()
				b.previewLines = nil
			} else {
				b.previewErr = ""
				b.previewLines = mm.lines
			}
		}
		return m, nil, true

	case browsePreviewCloseMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			p.browse.previewPath, p.browse.previewLines, p.browse.previewErr = "", nil, ""
		}
		return m, nil, true

	case browseKillMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			b := p.browse
			if b.client != nil && b.cursor >= 0 && b.cursor < len(b.jobIDs) {
				return m, killJobCmd(mm.id, b.client, b.path, b.jobIDs[b.cursor]), true
			}
		}
		return m, nil, true

	case browseKillResultMsg:
		if p := m.find(mm.id); p != nil && p.browse != nil {
			if mm.err != nil {
				p.browse.killMsg = "kill: " + mm.err.Error()
			} else {
				p.browse.killMsg = "kill: sent"
			}
		}
		return m, nil, true
	}
	return m, nil, false
}

// ---- view ----

// browseNode renders a browsing pane's content: the connection-owning
// companion widget (browseConnNode) plus whichever of connect-error /
// preview / listing is current. Wrapped in a Box so the connection
// closer survives swaps between listing and preview mode — see
// browseConnNode's own doc comment for why it can't live on either of
// those directly.
func browseNode(p *paneState) tui.Node {
	id := p.id
	b := p.browse

	var body tui.Node
	switch {
	case b.connErr != "":
		body = widget.List([]string{"connect error: " + b.connErr}, 0,
			widget.ListOptions{Theme: style.DefaultDark(), Frameless: true}, nil).Key(paneKey(id, "browselist"))
	case b.previewPath != "":
		body = browsePreviewContentNode(id, b)
	default:
		body = browseListNode(id, b)
	}

	return tui.Box(layout.Vertical,
		tui.Child(layout.Fill(1), body),
		// Length(0): browseConnNode paints nothing (see its own doc
		// comment) — it only needs to exist in the tree so tui's
		// reconciler disposes it (closing the client) when this pane's
		// Node stops appearing at all, the same convention widget.
		// Terminal's own Close/pty.Close already relies on.
		tui.Child(layout.Length(0), browseConnNode(p)),
	).Key(paneKey(id, "browsewrap"))
}

func browseListNode(id int, b *browseState) tui.Node {
	var items []string
	switch {
	case b.listErr != "":
		items = []string{"error: " + b.listErr}
	case b.jobRows != nil:
		items = append([]string(nil), b.jobRows...)
	case b.sessionRows != nil:
		items = append([]string(nil), b.sessionRows...)
	default:
		if len(b.path) > 0 {
			items = append(items, "..")
		}
		for _, e := range b.entries {
			items = append(items, formatBrowseEntry(e))
		}
	}
	if len(items) == 0 {
		if b.client == nil {
			items = []string{"(connecting...)"}
		} else {
			items = []string{"(empty)"}
		}
	}

	isJobTable := b.jobRows != nil
	return widget.List(items, b.cursor, widget.ListOptions{Theme: style.DefaultDark(), Frameless: true},
		func(e input.Event) tui.Msg {
			switch ev := e.(type) {
			case input.KeyEvent:
				switch {
				case ev.Key == input.KeyUp:
					return browseMoveMsg{id: id, delta: -1}
				case ev.Key == input.KeyDown:
					return browseMoveMsg{id: id, delta: 1}
				case ev.Key == input.KeyEnter:
					return browseEnterMsg{id: id}
				case ev.Key == input.KeyBackspace:
					return browseUpMsg{id: id}
				case ev.Rune == 'r':
					return browseRefreshMsg{id: id}
				case ev.Rune == 'k' && isJobTable:
					return browseKillMsg{id: id}
				}
			case input.MouseEvent:
				// List already translates a click's Y into the clicked
				// item's index (see widget.List.HandleEvent) — ev.Y here
				// is that index, not a screen row.
				if ev.Button == input.MouseLeft && !ev.Drag {
					return browseClickMsg{id: id, index: ev.Y}
				}
			}
			return nil
		}).Key(paneKey(id, "browselist"))
}

func formatBrowseEntry(e p9.Stat) string {
	if e.Qid.IsDir() {
		return e.Name + "/"
	}
	return fmt.Sprintf("%-10d %s", e.Length, e.Name)
}

// previewScrollStep is the mouse wheel's line-at-a-time scroll amount
// for a file preview — matches help.go's own helpScrollStep.
const previewScrollStep = 3

type browsePreviewProps struct {
	id    int
	path  string
	lines []string
}

// browsePreviewContentNode is a plain, read-only scrollable text
// viewer for one 9P file's content — modeled directly on help.go's
// helpWidget (same top-anchored scrollOffset, same PgUp/PgDown/wheel
// handling), the same choice 9sh's own browser.go made for the
// equivalent view and for the same reason (see that file's own doc
// comment on why sharing code with helpWidget isn't worth it: this
// needs to be a per-pane Component keyed by id, not a single app-wide
// instance behind a Modal).
func browsePreviewContentNode(id int, b *browseState) tui.Node {
	lines := b.previewLines
	if b.previewErr != "" {
		lines = []string{"error: " + b.previewErr}
	}
	if len(lines) == 0 {
		lines = []string{"(empty)"}
	}
	return tui.Component(paneKey(id, "browsepreview"), browsePreviewProps{id: id, path: b.previewPath, lines: lines}, func() tui.Widget {
		return &browsePreviewWidget{}
	}).Key(paneKey(id, "browsepreview"))
}

type browsePreviewWidget struct {
	props        browsePreviewProps
	scrollOffset int
	lastHeight   int
}

func (w *browsePreviewWidget) Reconcile(props any) bool {
	w.props = props.(browsePreviewProps)
	return true
}

func (w *browsePreviewWidget) Paint(p *cell.Painter) {
	width, height := p.Size()
	if width <= 0 || height <= 0 {
		return
	}
	w.lastHeight = height
	lines := w.props.lines
	maxStart := max0(len(lines) - height)
	start := clamp(w.scrollOffset, 0, maxStart)
	end := min(start+height, len(lines))
	for y, line := range lines[start:end] {
		p.Text(0, y, line, cell.Style{})
	}
}

func (w *browsePreviewWidget) HandleEvent(e input.Event) tui.Cmd {
	id := w.props.id
	switch ev := e.(type) {
	case input.MouseEvent:
		switch ev.Button {
		case input.MouseWheelUp:
			w.scrollOffset = max0(w.scrollOffset - previewScrollStep)
		case input.MouseWheelDown:
			w.scrollOffset += previewScrollStep
		}
	case input.KeyEvent:
		switch {
		case ev.Key == input.KeyEsc, ev.Key == input.KeyBackspace:
			return func() tui.Msg { return browsePreviewCloseMsg{id: id} }
		case ev.Key == input.KeyPgUp:
			w.scrollOffset = max0(w.scrollOffset - max0(w.lastHeight-1))
		case ev.Key == input.KeyPgDown:
			w.scrollOffset += max0(w.lastHeight - 1)
		}
	}
	return nil
}

func (w *browsePreviewWidget) Focusable() bool { return true }
func (w *browsePreviewWidget) SetFocused(bool) {}

// browseConnNode is a permanently-mounted, invisible companion to a
// browsing pane's visible content — its only job is closing the
// pane's 9P client connection when the pane itself is disposed. tui's
// reconciler calls Close() on any widget implementing io.Closer that
// stops appearing in the tree at all (see widget.Terminal's own
// Close/pty.Close, the existing convention this mirrors); kept
// separate from the visible list/preview widgets because those are
// deliberately disposed/recreated when swapping between listing and
// preview mode (see browsePreviewContentNode's own doc comment) — the
// client connection must survive that swap, so it can't live on
// either of them.
func browseConnNode(p *paneState) tui.Node {
	return tui.Component(paneKey(p.id, "browseconn"), p.browse.client, func() tui.Widget {
		return &browseConnWidget{}
	})
}

type browseConnWidget struct {
	client *client.Client
}

func (w *browseConnWidget) Reconcile(props any) bool {
	w.client, _ = props.(*client.Client)
	return false
}
func (w *browseConnWidget) Paint(p *cell.Painter)           {}
func (w *browseConnWidget) HandleEvent(input.Event) tui.Cmd { return nil }
func (w *browseConnWidget) Focusable() bool                 { return false }
func (w *browseConnWidget) SetFocused(bool)                 {}
func (w *browseConnWidget) Close() error {
	if w.client != nil {
		return w.client.Close()
	}
	return nil
}
