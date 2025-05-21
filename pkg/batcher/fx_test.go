package batcher_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/atomic"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/NSXBet/batcher/pkg/batcher"
)

type BatchItem struct {
	ID   int
	Name string
}

type Processor struct {
	counter *atomic.Uint32
}

func NewProcessor() *Processor {
	return &Processor{counter: atomic.NewUint32(0)}
}

func (p *Processor) Process(items []*BatchItem) error {
	p.counter.Add(uint32(len(items)))

	return nil
}

func TestProvideBatcherInFX(t *testing.T) {
	// Create a variable to hold the batcher
	var (
		b *batcher.Batcher[*BatchItem]
		p *Processor
	)

	// Create test app with batcher
	app := fxtest.New(t,
		fx.Provide(NewProcessor),
		batcher.ProvideBatcherInFX[*BatchItem](
			func(processor *Processor) batcher.Processor[*BatchItem] {
				return processor.Process
			},
			2,                    // batch size
			time.Millisecond*100, // batch interval
		),
		fx.Populate(&b, &p), // Populate the batcher variable
	)

	// Start the app
	app.RequireStart()
	require.NotNil(t, b, "batcher should be populated")

	// Add some items to the batcher
	b.Add(&BatchItem{ID: 1, Name: "item1"})
	b.Add(&BatchItem{ID: 2, Name: "item2"})

	// Wait for processor to be called
	require.Eventually(t, func() bool {
		return p.counter.Load() == 2
	}, time.Second, time.Millisecond*100)

	// Stop the app
	app.RequireStop()

	// Verify batcher is closed
	require.True(t, b.IsClosed(), "batcher should be closed after app stop")
	require.Equal(t, uint32(2), p.counter.Load(), "processor should have processed 2 items")
}
