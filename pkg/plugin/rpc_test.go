package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	goPlugin "github.com/hashicorp/go-plugin"
)

type rpcTestPlugin struct {
	host Host
}

func (plugin *rpcTestPlugin) Manifest() Manifest {
	return Manifest{
		ID:            "rpc-test",
		Name:          "RPC test",
		Version:       "1.0.0",
		SDKVersion:    "0.1",
		APIVersion:    APIVersion,
		Author:        "test",
		License:       "MIT",
		RepositoryURL: "https://example.com/rpc-test",
		Entrypoint:    "./cmd/rpc-test",
	}
}

func (plugin *rpcTestPlugin) Initialize(ctx context.Context, host Host) error {
	plugin.host = host
	var value map[string]any
	return host.Call(ctx, "test.initialize", nil, &value)
}

func (plugin *rpcTestPlugin) Before(_ context.Context, operation Operation) (OperationResult, error) {
	return OperationResult{Allowed: true, Payload: operation.Payload}, nil
}

type rpcTestHost struct{}

func (host *rpcTestHost) Call(_ context.Context, method string, _ any, response any) error {
	if method != "test.initialize" {
		return errors.New("unexpected host method")
	}
	switch value := response.(type) {
	case *json.RawMessage:
		*value = json.RawMessage(`{"ready":true}`)
	case *map[string]any:
		*value = map[string]any{"ready": true}
	default:
		return errors.New("unexpected response type")
	}
	return nil
}

func (host *rpcTestHost) OpenStream(context.Context, string, any) (io.ReadWriteCloser, error) {
	return nil, errors.New("stream not implemented in test")
}

func TestRPCPluginCanInitializeAndCallHooks(t *testing.T) {
	client, server := goPlugin.TestPluginRPCConn(t, map[string]goPlugin.Plugin{
		PluginName: &RPCPlugin{Impl: &rpcTestPlugin{}},
	}, nil)
	defer client.Close()
	_ = server

	dispensed, err := client.Dispense(PluginName)
	if err != nil {
		t.Fatalf("Dispense: %v", err)
	}
	pluginClient, ok := dispensed.(*Client)
	if !ok {
		t.Fatalf("Dispense returned %T, want *Client", dispensed)
	}
	if err := pluginClient.AttachHost(context.Background(), &rpcTestHost{}); err != nil {
		t.Fatalf("AttachHost: %v", err)
	}
	manifest, err := pluginClient.Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if manifest.ID != "rpc-test" {
		t.Fatalf("manifest id = %q, want rpc-test", manifest.ID)
	}
	result, err := pluginClient.Before(context.Background(), Operation{
		Name:    "files.list",
		Payload: json.RawMessage(`{"path":"/"}`),
	})
	if err != nil {
		t.Fatalf("Before: %v", err)
	}
	if !result.Allowed || string(result.Payload) != `{"path":"/"}` {
		t.Fatalf("unexpected hook result: %+v", result)
	}
}
