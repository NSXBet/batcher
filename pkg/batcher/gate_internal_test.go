package batcher

import (
	"testing"
	"time"
)

// TestGateRejectionPathSignalsQuiescence forces the lost-wakeup interleaving that
// only the rejection path can produce:
//
//  1. an entrant increments the gate;
//  2. the coordinator seals, observes a NON-ZERO gate, and commits to waiting;
//  3. the entrant's post-increment re-check sees the seal and decrements to zero.
//
// If that decrement bypasses leave(), nobody signals quiescence and the waiter
// blocks forever. This is distinct from the last-leaver-before-seal case, because
// there the coordinator's own check finds the gate empty and signals; here it has
// already found the gate occupied, so only leave() can signal.
//
// The interleaving is forced with a hook rather than hoped for: a test that only
// sometimes exercises the hazard is not a regression test. Sabotaging enter()'s
// leave() call must make this hang.
func TestGateRejectionPathSignalsQuiescence(t *testing.T) {
	t.Parallel()

	const trials = 500

	for trial := range trials {
		g := newAdmissionGate()

		incremented := make(chan struct{})
		sealed := make(chan struct{})

		// Pause the entrant between its increment and its re-check.
		g.afterIncrement = func() {
			close(incremented)
			<-sealed
		}

		entered := make(chan bool, 1)

		go func() { entered <- g.enter() }()

		<-incremented

		// The coordinator seals while the gate is occupied, so its own check cannot
		// signal quiescence. Only the entrant's exit can.
		g.seal()
		g.checkEmpty()

		close(sealed)

		if <-entered {
			t.Fatalf("trial %d: entry must be rejected once sealing has begun", trial)
		}

		select {
		case <-g.empty():
		case <-time.After(2 * time.Second):
			t.Fatalf("trial %d: quiescence was never signalled; the rejection path's "+
				"gate decrement bypassed leave()", trial)
		}
	}
}
