package batcher_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/NSXBet/batcher/pkg/batcher"
)

// RequestHandler handles incoming requests and enqueues them to the batcher
type RequestHandler struct {
	batcher *batcher.Batcher[*BatchItem]
}

// NewRequestHandler creates a new request handler with batcher dependency
func NewRequestHandler(b *batcher.Batcher[*BatchItem]) *RequestHandler {
	return &RequestHandler{
		batcher: b,
	}
}

// HandleRequest processes a single request by enqueueing it to the batcher
func (h *RequestHandler) HandleRequest(id int, name string) error {
	h.batcher.Add(&BatchItem{
		ID:   id,
		Name: name,
	})
	return nil
}

func TestREADMEExample(t *testing.T) {
	// Create variables to hold dependencies
	var (
		handler   *RequestHandler
		processor *Processor
	)

	// Create test app with batcher
	app := fxtest.New(t,
		fx.Provide(NewProcessor),
		// Add the batcher module
		batcher.ProvideBatcherInFX[*BatchItem](
			func(processor *Processor) batcher.Processor[*BatchItem] {
				return processor.Process
			},
			2,                    // batch size
			time.Millisecond*100, // batch interval
		),
		// Provide the request handler
		fx.Provide(NewRequestHandler),
		// Populate dependencies
		fx.Populate(&handler, &processor),
	)

	// Start the app
	app.RequireStart()
	require.NotNil(t, handler, "handler should be populated")

	// Handle some requests
	for i := 0; i < 10; i++ {
		err := handler.HandleRequest(i, "test-item")

		require.NoError(t, err)
	}

	// Wait for processor to be called
	require.Eventually(t, func() bool {
		return processor.counter.Load() == 10
	}, time.Second, time.Millisecond*100)

	// Stop the app
	app.RequireStop()

	// Verify batcher is closed
	require.True(t, handler.batcher.IsClosed(), "batcher should be closed after app stop")
	require.Equal(
		t,
		uint32(10),
		processor.counter.Load(),
		"processor should have processed 10 items",
	)
}
