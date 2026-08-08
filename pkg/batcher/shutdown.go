package batcher

import (
	"errors"
	"fmt"
)

// ShutdownIncompleteError reports that a shutdown wait expired before the drain
// finished.
//
// It does NOT mean work was discarded. The drain continues in the background, and
// a caller who wants to keep waiting can call Shutdown again with a fresh context;
// that second call waits on the same drain rather than restarting it.
type ShutdownIncompleteError struct {
	// Pending is a conservative drain obligation, not a queue depth.
	//
	// Three things inflate it relative to "items still queued", and callers who
	// treat it as a precise figure will be misled:
	//
	//   - it counts items currently inside a processor call;
	//   - it counts publishers that have reserved but not yet published, which may
	//     yet abort;
	//   - it is sampled from atomics without a lock, so it is a snapshot.
	//
	// It is an exact count of accepted-but-unfinished work only once
	// PublishersInGate is zero. For queue depth, use Stats().Queued.
	Pending int

	// PublishersInGate is how many publishers were still inside the publication
	// window when the wait expired. Zero means Pending is exact.
	PublishersInGate int

	// Cause is the caller's context error: context.DeadlineExceeded or
	// context.Canceled.
	Cause error
}

func (e *ShutdownIncompleteError) Error() string {
	return fmt.Sprintf(
		"batcher shutdown incomplete: %d pending, %d publishers still admitting: %v",
		e.Pending, e.PublishersInGate, e.Cause,
	)
}

// Unwrap exposes the context error, so errors.Is(err, context.DeadlineExceeded)
// works as callers expect.
func (e *ShutdownIncompleteError) Unwrap() error {
	return e.Cause
}

// Is reports ErrTimeout equivalence, so callers written against Close's original
// error keep working after moving to Shutdown.
func (e *ShutdownIncompleteError) Is(target error) bool {
	return errors.Is(target, ErrTimeout)
}

// ProcessorPanicError reports that a processor panicked and the panic was
// recovered.
//
// Batcher recovers processor panics because it owns goroutines the caller cannot
// reach: an unrecovered panic there would crash the whole process and discard every
// other queued item, and the caller has no way to install its own recover. The
// panic value and stack are preserved so the bug remains debuggable rather than
// being silently swallowed.
type ProcessorPanicError struct {
	// Value is whatever was passed to panic().
	Value any

	// Stack is the stack trace captured at recovery.
	Stack []byte
}

func (e *ProcessorPanicError) Error() string {
	return fmt.Sprintf("batcher processor panicked: %v", e.Value)
}
