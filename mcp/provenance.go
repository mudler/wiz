package mcp

import (
	"context"

	"github.com/mudler/nib/provenance"
)

func protectExternal(ctx context.Context, classifier provenance.Classifier, kind, locator, text string) provenance.Envelope {
	return provenance.NewExternal(ctx, "", kind, locator, text, classifier)
}
