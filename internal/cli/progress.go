package cli

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Scan progress reporting
//
// A long organization scan is silent for minutes: ingestion, then metric
// enrichment across every project, then pricing, then rule evaluation. The
// Progress reporter emits one line per phase on STDERR so an operator can tell
// a slow scan from a hung one. It is a status channel, not a log: it is
// independent of --log-level, and it never touches stdout — which belongs to
// the report and is piped into other tools (`tellury scan --format json | jq`
// must stay pure).
//
// The constraints matter more than the animation:
//
//   - STDERR ONLY. Every write goes to the writer runScan is handed as errOut.
//     stdout is the report; a progress line there corrupts a JSON or CSV
//     stream piped into another tool.
//
//   - NO ANSI, EVER. This package never emits a carriage return or an escape
//     sequence, in any mode. On an interactive terminal the lines are printed
//     more often; when stderr is a pipe, a file, a buffer or a CI log they
//     degrade to plain periodic lines (or stay silent in auto mode). Nothing a
//     log file has to parse can be garbled by an escape code.
//
//   - LOCK-FREE IN THE HOT PATH. Phase.Set is invoked from worker goroutines
//     whose bounded concurrency is the whole point (metric fetches, rule
//     evaluations). Its hot path is two atomic stores plus one CompareAndSwap
//     against a per-phase throttle timestamp — no mutex. A mutex is taken only
//     when a line actually prints (at most once per interval), so reporting
//     progress can never serialize the work it reports on.
// ─────────────────────────────────────────────────────────────────────────────

// Progress writes throttled, phase-oriented status lines to one writer.
// Begin returns a Phase handle for the phase's worker goroutines to report
// through, or nil when progress is disabled — call sites read
// "ph := prog.Begin(...)" and nil-check ph exactly once.
type Progress struct {
	w        io.Writer
	enabled  bool
	interval time.Duration
	mu       sync.Mutex // guards the writer; taken only when a line prints
}

// Enabled reports whether the reporter will emit lines.
func (p *Progress) Enabled() bool { return p != nil && p.enabled }

// newProgress resolves --progress/TELLURY_PROGRESS against whether errOut is a
// terminal (the mode is the already-normalized value from config.Scan):
//
//	auto (default): report only when errOut is an interactive terminal;
//	      silent when it is a pipe, file or CI log (the operator opted into
//	      nothing, so nothing is scribbled into a log).
//	on             : always report. A non-terminal errOut degrades to plain
//	              periodic lines — throttled harder, never animated, never
//	              ANSI.
//	off            : never report.
func newProgress(w io.Writer, mode string) *Progress {
	p := &Progress{w: w, interval: time.Second}
	switch mode {
	case "on":
		p.enabled = true
		if !isTerminal(w) {
			p.interval = 5 * time.Second // degrade to plain periodic lines
		}
	case "off":
		// enabled stays false.
	default: // "auto"
		p.enabled = isTerminal(w)
	}
	return p
}

// isTerminal reports whether w is a character device — an interactive
// terminal — as opposed to a pipe, a file, a buffer, or anything else. It is
// the dependency-free, portable "is a TTY" check: a character device is the
// only place animated output would even be legible, and this reporter never
// emits carriage returns or escape codes anywhere.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Begin starts a progress phase. label names the phase ("asset discovery",
// "metric enrichment", "pricing catalogue", "rule evaluation"); unit names the
// work unit its denominator counts ("fetches", "services", "rules"), or ""
// when the phase has no known total. Returns nil when progress is disabled.
func (p *Progress) Begin(label, unit string) *Phase {
	if p == nil || !p.enabled {
		return nil
	}
	ph := &Phase{prog: p, label: label, unit: unit, start: time.Now()}
	ph.printStart()
	return ph
}

// Phase is one named stage of a scan. Its Set method is safe to call from any
// number of worker goroutines concurrently; it is deliberately lock-free (two
// atomic stores plus a CompareAndSwap on the phase's own throttle timestamp)
// so reporting progress can never serialize the bounded-concurrency work it
// reports on.
type Phase struct {
	prog  *Progress
	label string
	unit  string
	start time.Time

	total     atomic.Int64 // established by the first Set call
	done      atomic.Int64
	lastPrint atomic.Int64 // unix nanos of the phase's last printed line
}

// Set records that done of total units have completed. The first call in a
// phase establishes total (later calls ignore a differing total), so the
// denominator comes from the code that knows it: metric enrichment reports
// (key, project) fetches, rule evaluation reports rules. A count line prints
// at most once per Progress.interval.
func (ph *Phase) Set(done, total int) {
	if ph == nil || !ph.prog.Enabled() {
		return
	}
	if total > 0 {
		ph.total.CompareAndSwap(0, int64(total))
	}
	ph.done.Store(int64(done))

	now := time.Now().UnixNano()
	last := ph.lastPrint.Load()
	if now-last >= int64(ph.prog.interval) && ph.lastPrint.CompareAndSwap(last, now) {
		ph.print()
	}
}

// End prints the phase's final line. detail is an optional suffix ("1,204
// resources", "14 projects") rendered after the elapsed time.
func (ph *Phase) End(detail string) {
	if ph == nil || !ph.prog.Enabled() {
		return
	}
	ph.prog.mu.Lock()
	defer ph.prog.mu.Unlock()

	total, done := ph.total.Load(), ph.done.Load()
	line := fmt.Sprintf("tellury: %s: done", ph.label)
	if total > 0 {
		line += fmt.Sprintf(" %d/%d", done, total)
		if ph.unit != "" {
			line += " " + ph.unit
		}
	}
	line += fmt.Sprintf(" (%s", progressDuration(time.Since(ph.start)))
	if detail != "" {
		line += ", " + detail
	}
	line += ")\n"
	fmt.Fprint(ph.prog.w, line)
}

// printStart emits the phase's opening line. It runs on the scan's own
// goroutine (Begin), so it needs no throttle.
func (ph *Phase) printStart() {
	ph.prog.mu.Lock()
	defer ph.prog.mu.Unlock()
	fmt.Fprintf(ph.prog.w, "tellury: %s: started\n", ph.label)
}

// print emits a throttled count line. Called only from Set's CAS winner; it
// takes the writer mutex because it actually writes.
func (ph *Phase) print() {
	ph.prog.mu.Lock()
	defer ph.prog.mu.Unlock()

	total, done := ph.total.Load(), ph.done.Load()
	elapsed := progressDuration(time.Since(ph.start))
	if total > 0 {
		if ph.unit != "" {
			fmt.Fprintf(ph.prog.w, "tellury: %s: %d/%d %s (%s)\n",
				ph.label, done, total, ph.unit, elapsed)
			return
		}
		fmt.Fprintf(ph.prog.w, "tellury: %s: %d/%d (%s)\n", ph.label, done, total, elapsed)
		return
	}
	fmt.Fprintf(ph.prog.w, "tellury: %s: %d (%s)\n", ph.label, done, elapsed)
}

// progressDuration renders a duration compactly for status lines: rounded to
// milliseconds once the phase passes one, to microseconds below so a fast
// phase reads "1.2ms" rather than "0s".
func progressDuration(d time.Duration) string {
	if d >= time.Millisecond {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(time.Microsecond).String()
}

// progressCount renders "1 resource" / "3 resources" for a phase's End detail.
func progressCount(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// catalogueProgressSetter is implemented by pricers that load a pricing
// catalogue lazily (the live Cloud Billing CatalogPricer). It lets the CLI
// report that load as its own progress phase without importing the concrete
// pricing package. final is true on the last call whether the load succeeded
// or not.
type catalogueProgressSetter interface {
	SetCatalogueProgress(func(done, total int, final bool))
}
