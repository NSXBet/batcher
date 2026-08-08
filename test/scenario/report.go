package scenario

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"
)

// Environment captures everything needed to interpret a report. Latency numbers
// are meaningless without it: the same scenario on a different runner, Go
// version, or GOMAXPROCS is a different measurement.
type Environment struct {
	GoVersion  string
	OS         string
	Arch       string
	GOMAXPROCS int
	NumCPU     int
}

// CurrentEnvironment reads the environment of the running process.
func CurrentEnvironment() Environment {
	return Environment{
		GoVersion:  runtime.Version(),
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		NumCPU:     runtime.NumCPU(),
	}
}

func (e Environment) String() string {
	return fmt.Sprintf("go=%s os=%s arch=%s gomaxprocs=%d cpus=%d",
		e.GoVersion, e.OS, e.Arch, e.GOMAXPROCS, e.NumCPU)
}

// SubMillisecondWindow reports whether a configured window is small enough that
// results depend heavily on host timer and scheduler behaviour. Such runs are
// reported but must not be used as portable pass/fail evidence.
func SubMillisecondWindow(window time.Duration) bool {
	return window < time.Millisecond
}

// WriteReport renders results as a fixed-width table with environment metadata.
// Invalid runs are labelled rather than silently mixed with valid ones.
func WriteReport(w io.Writer, results []Result) error {
	env := CurrentEnvironment()

	if _, err := fmt.Fprintf(w, "environment: %s\n\n", env); err != nil {
		return err
	}

	header := fmt.Sprintf("%-26s %-8s %-9s %8s %8s %9s %9s %8s %10s %9s %7s",
		"scenario", "window", "arrival", "p50", "p99", "p99.9", "max",
		"batch", "work_pk", "calls/s", "valid")

	if _, err := fmt.Fprintln(w, header); err != nil {
		return err
	}

	if _, err := fmt.Fprintln(w, strings.Repeat("-", len(header))); err != nil {
		return err
	}

	for _, r := range results {
		valid := "yes"
		if !r.LatenessValid {
			valid = "LATE"
		} else if SubMillisecondWindow(r.Config.BatchInterval) {
			valid = "env"
		}

		if _, err := fmt.Fprintf(w,
			"%-26s %-8s %-9s %8s %8s %9s %9s %8.0f %10d %9.0f %7s\n",
			truncate(r.Config.Name, 26),
			r.Config.BatchInterval,
			truncate(r.Config.Arrival.Name, 9),
			round(r.EndToEnd.P50),
			round(r.EndToEnd.P99),
			round(r.EndToEnd.P999),
			round(r.EndToEnd.Max),
			r.MeanBatchSize,
			r.PendingWorkPeak,
			r.DownstreamPerSec,
			valid,
		); err != nil {
			return err
		}
	}

	return nil
}

func round(d time.Duration) time.Duration {
	return d.Round(time.Microsecond)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	return s[:limit]
}
