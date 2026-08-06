package batcher

import (
	"sync"
	"sync/atomic"
)

// lifecycle state values. Exactly one holds at any time.
const (
	stateNew      int32 = iota // constructed, processing not started
	stateRunning               // processing started, admitting
	stateSealing               // admission closed, publishers still leaving the gate
	stateDraining              // gate empty, accepted work still processing
	stateClosed                // terminal: all owned goroutines have exited
)

// admissionGate serialises "is admission still open?" against shutdown without
// putting a mutex on the enqueue path.
//
// The gate counts publishers currently inside the publication window. Its purpose
// is to let the shutdown coordinator prove that no publisher can still be
// mid-publish, which is what makes it safe to declare intake finished.
//
// The protocol is subtle in two places, and both have dedicated tests:
//
//  1. enter() increments, then RE-CHECKS the seal. Without the re-check, a
//     publisher that read "not sealed" could increment after the coordinator had
//     already observed an empty gate, and publish into a queue nobody will drain.
//  2. every decrement routes through leave(), including enter()'s own rejection
//     path. A bare decrement there loses the wakeup: the coordinator can observe a
//     non-zero gate and commit to waiting, and the rejecting publisher can then
//     drop the count to zero without signalling, hanging shutdown forever.
type admissionGate struct {
	state  atomic.Int32
	sealed atomic.Bool

	// afterIncrement is used only by package-internal tests to force the race
	// between incrementing the gate and re-checking the seal. It is nil in
	// production and has no hot-path effect beyond one nil check.
	afterIncrement func()

	// publishers counts goroutines between a successful enter() and its leave().
	publishers atomic.Int64

	// sealCh is closed once when sealing begins, releasing publishers parked on a
	// full bounded queue. Closing is used rather than sending so every parked
	// publisher observes it.
	sealCh    chan struct{}
	sealOnce  sync.Once
	emptyCh   chan struct{}
	emptyOnce sync.Once
}

func newAdmissionGate() *admissionGate {
	return &admissionGate{
		sealCh:  make(chan struct{}),
		emptyCh: make(chan struct{}),
	}
}

// enter attempts to join the publication window.
//
// A successful enter must be paired with exactly one leave.
func (g *admissionGate) enter() bool {
	if g.sealed.Load() {
		return false
	}

	g.publishers.Add(1)

	if g.afterIncrement != nil {
		g.afterIncrement()
	}

	if g.sealed.Load() {
		// Sealing raced our increment. Route the decrement through leave() so the
		// coordinator cannot miss the transition to zero.
		g.leave()

		return false
	}

	return true
}

// leave exits the publication window, signalling quiescence when it is the last
// publisher out after sealing.
func (g *admissionGate) leave() {
	if g.publishers.Add(-1) == 0 && g.sealed.Load() {
		g.signalEmpty()
	}
}

// seal closes admission. It is idempotent.
//
// The caller must follow this with checkEmpty, which covers the interleaving where
// the last publisher left before sealed was stored and therefore never signalled.
func (g *admissionGate) seal() {
	g.sealed.Store(true)
	g.sealOnce.Do(func() { close(g.sealCh) })
}

// checkEmpty is the coordinator's own quiescence check.
//
// This is mandatory, not an optimisation: leave() only signals when it observes
// sealed, so if the final publisher left before sealing, nobody would ever signal.
func (g *admissionGate) checkEmpty() {
	if g.publishers.Load() == 0 && g.sealed.Load() {
		g.signalEmpty()
	}
}

// signalEmpty arms the quiescence latch exactly once. It closes a channel rather
// than sending, so it never blocks and every waiter observes it.
func (g *admissionGate) signalEmpty() {
	g.emptyOnce.Do(func() { close(g.emptyCh) })
}

// empty returns the latch that closes once no publisher remains in the window.
func (g *admissionGate) empty() <-chan struct{} {
	return g.emptyCh
}

// sealed reports whether admission is closed.
func (g *admissionGate) isSealed() bool {
	return g.sealed.Load()
}

// inGate reports how many publishers are inside the publication window.
func (g *admissionGate) inGate() int64 {
	return g.publishers.Load()
}
