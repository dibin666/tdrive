package drive

import (
	"context"
	"encoding/json"
	"fmt"

	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

type pluginHookHolder struct {
	hooks tdriveplugin.Hooks
}

// SetPluginHooks enables the optional operation bridge. The host calls this
// only after at least one plugin has successfully started; a nil value is the
// original no-plugin path and avoids JSON encoding, locks, and RPC calls.
func (service *Service) SetPluginHooks(hooks tdriveplugin.Hooks) {
	if hooks == nil {
		service.pluginHooks.Store(nil)
		return
	}
	service.pluginHooks.Store(&pluginHookHolder{hooks: hooks})
}

func (service *Service) beforePluginOperation(ctx context.Context, name string, payload any, target any) (tdriveplugin.Operation, error) {
	if tdriveplugin.IsHostCall(ctx) {
		return tdriveplugin.Operation{Name: name}, nil
	}
	holder := service.pluginHooks.Load()
	if holder == nil || holder.hooks == nil {
		return tdriveplugin.Operation{Name: name}, nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return tdriveplugin.Operation{}, fmt.Errorf("encode plugin operation %q: %w", name, err)
	}
	operation := tdriveplugin.Operation{
		Name:    name,
		UserID:  tdriveplugin.UserIDFromContext(ctx),
		Payload: data,
	}
	result, err := holder.hooks.Before(ctx, operation)
	if err != nil {
		return tdriveplugin.Operation{}, err
	}
	if !result.Allowed {
		if result.Error == "" {
			result.Error = "operation rejected by plugin"
		}
		return tdriveplugin.Operation{}, fmt.Errorf("%s", result.Error)
	}
	if len(result.Payload) > 0 {
		data = result.Payload
	}
	if target != nil && len(data) > 0 {
		if err := json.Unmarshal(data, target); err != nil {
			return tdriveplugin.Operation{}, fmt.Errorf("decode plugin operation %q: %w", name, err)
		}
	}
	operation.Payload = data
	return operation, nil
}

func (service *Service) afterPluginOperation(ctx context.Context, operation tdriveplugin.Operation) {
	if tdriveplugin.IsHostCall(ctx) {
		return
	}
	if holder := service.pluginHooks.Load(); holder != nil && holder.hooks != nil {
		holder.hooks.After(ctx, operation)
	}
}
