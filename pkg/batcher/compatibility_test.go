package batcher_test

import (
	"testing"
	"time"

	"github.com/NSXBet/batcher/pkg/batcher"
	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
)

// Compile-level compatibility checks for v0.3.0.
//
// The migration guide in docs/improvements/compatibility.md claims certain v0.2.x
// call shapes still compile. These tests are that claim, executed: if a future change
// breaks one of them, this file stops building rather than the guide quietly becoming
// wrong.
//
// Deliberately NOT asserted here: assignment through Config(), which v0.2.x allowed
// and which no longer compiles. That break is intentional — it was a data race — and
// is declared in the migration guide instead.

type legacyItem struct {
	ID int
}

type legacyProcessor struct {
	seen int
}

func newLegacyProcessor() *legacyProcessor {
	return &legacyProcessor{}
}

func (p *legacyProcessor) Process(items []*legacyItem) error {
	p.seen += len(items)

	return nil
}

// TestLegacyConstructionStillCompiles pins the constructor and option shapes from
// the v0.2.x README.
func TestLegacyConstructionStillCompiles(t *testing.T) {
	t.Parallel()

	b := batcher.New[*legacyItem](
		batcher.WithBatchSize[*legacyItem](100),
		batcher.WithBatchInterval[*legacyItem](time.Second),
		batcher.WithProcessor(func(items []*legacyItem) error {
			return nil
		}),
	)

	b.Add(&legacyItem{ID: 1})

	require.NoError(t, b.Join(10*time.Second))
	require.NoError(t, b.Close())
	require.True(t, b.IsClosed())
}

// TestLegacyConfigReadsStillCompile pins that read-only Config() access is
// unaffected by the pointer-to-value change. Only assignment broke.
func TestLegacyConfigReadsStillCompile(t *testing.T) {
	t.Parallel()

	b := batcher.New[*legacyItem](
		batcher.WithBatchSize[*legacyItem](250),
		batcher.WithBatchInterval[*legacyItem](2*time.Second),
	)

	defer func() { require.NoError(t, b.Close()) }()

	// Field access through the call, as v0.2.x code did.
	require.Equal(t, 250, b.Config().BatchSize)
	require.Equal(t, 2*time.Second, b.Config().BatchInterval)
	require.NotNil(t, b.Config().ProcessorFunc)

	// Assigning the result to a local also still works; it is simply a copy now.
	cfg := b.Config()
	require.Equal(t, 250, cfg.BatchSize)
}

// TestLegacySkipAutoStartStillCompiles pins the manual-start lifecycle shape.
func TestLegacySkipAutoStartStillCompiles(t *testing.T) {
	t.Parallel()

	b := batcher.New[*legacyItem](
		batcher.WithSkipAutoStart[*legacyItem](),
		batcher.WithBatchSize[*legacyItem](2),
		batcher.WithBatchInterval[*legacyItem](5*time.Millisecond),
		batcher.WithProcessor(func([]*legacyItem) error { return nil }),
	)

	b.Start()

	b.Add(&legacyItem{ID: 1})
	b.Add(&legacyItem{ID: 2})

	require.NoError(t, b.Join(10*time.Second))
	require.NoError(t, b.Close())
}

// TestLegacyErrorsChannelStillCompiles pins the diagnostics consumption shape.
func TestLegacyErrorsChannelStillCompiles(t *testing.T) {
	t.Parallel()

	b := batcher.New[*legacyItem](
		batcher.WithBatchSize[*legacyItem](1),
		batcher.WithBatchInterval[*legacyItem](time.Millisecond),
		batcher.WithProcessor(func([]*legacyItem) error { return nil }),
	)

	drained := make(chan struct{})

	go func() {
		defer close(drained)

		for range b.Errors() {
		}
	}()

	b.Add(&legacyItem{ID: 1})

	require.NoError(t, b.Close())

	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("Errors() must still close on shutdown so a range loop terminates")
	}
}

// TestLegacyProvideBatcherInFXSignatureStillCompiles pins the Fx entry point that
// existing applications wire up. Adding options must not have required a positional
// parameter here.
func TestLegacyProvideBatcherInFXSignatureStillCompiles(t *testing.T) {
	t.Parallel()

	var (
		b *batcher.Batcher[*legacyItem]
		p *legacyProcessor
	)

	app := fxtest.New(t,
		fx.Provide(newLegacyProcessor),
		batcher.ProvideBatcherInFX[*legacyItem](
			func(processor *legacyProcessor) batcher.Processor[*legacyItem] {
				return processor.Process
			},
			2,
			50*time.Millisecond,
		),
		fx.Populate(&b, &p),
	)

	app.RequireStart()

	b.Add(&legacyItem{ID: 1})
	b.Add(&legacyItem{ID: 2})

	require.NoError(t, b.Join(10*time.Second))

	app.RequireStop()

	require.Equal(t, 2, p.seen)
}

// TestDefaultBatchIntervalStaysOneSecond pins the default's literal value, not just
// that Config reports whatever the constant happens to say.
//
// The 10ms decision record makes a strong latency case, and it is easy to read as a
// mandate to change this constant. It was deliberately not changed: at 1,000 items/s
// the measured downstream call rate moves from ~1/s to ~94/s, and that cost lands on
// callers who never configured an interval. Lowering the default is a behaviour change
// for every such caller, so it needs its own decision rather than arriving as a tidy-up.
//
// Asserting through Config as well as the constant covers both halves: the advertised
// value and the value New actually applies.
func TestDefaultBatchIntervalStaysOneSecond(t *testing.T) {
	t.Parallel()

	require.Equal(t, time.Second, batcher.DefaultBatchInterval,
		"DefaultBatchInterval must stay 1s; see docs/improvements/default-window.md, "+
			"which recommends 10ms as an explicit setting rather than as the default")

	b := batcher.New(batcher.WithProcessor(batcher.NoOpProcessor[int]))

	t.Cleanup(func() { _ = b.Close() })

	require.Equal(t, time.Second, b.Config().BatchInterval,
		"a batcher constructed without WithBatchInterval must use the 1s default")
}
