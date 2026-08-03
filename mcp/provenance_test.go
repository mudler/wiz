package mcp

import (
	"context"

	"github.com/mudler/nib/provenance"
)

type allowClassifier struct{}

func (allowClassifier) Classify(context.Context, string) ([]provenance.Span, error) {
	return nil, nil
}
