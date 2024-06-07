package test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type BatchItem struct {
	Key string
}

type Processor struct {
	logger *zap.Logger
}

func NewProcessor(t *testing.T) *Processor {
	t.Helper()

	logger, err := zap.NewDevelopment()
	require.NoError(t, err)

	return &Processor{
		logger: logger,
	}
}

func (p *Processor) Process(items []BatchItem) error {
	p.logger.Info("processing items", zap.Int("count", len(items)))

	return nil
}
