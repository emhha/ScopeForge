package modelutil

import (
	"fmt"

	"trpc.group/trpc-go/trpc-agent-go/model"
)

// ProjectProviderData preserves the native adapter's optional projection
// capability through product decorators. The adapter owns payload validation
// and decoding; wrappers must not recreate or flatten provider records.
func ProjectProviderData(inner model.Model, data *model.ProviderData) (model.Message, error) {
	if projector, ok := inner.(model.ProviderDataProjector); ok {
		return projector.ProjectProviderData(data)
	}
	return model.Message{}, fmt.Errorf("model %q does not support native provider data projection", inner.Info().Name)
}
