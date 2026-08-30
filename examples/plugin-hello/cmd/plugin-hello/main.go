package main

import (
	"context"

	tdriveplugin "github.com/dibin/tdrive/pkg/plugin"
)

type helloPlugin struct {
	host tdriveplugin.Host
}

func (plugin *helloPlugin) Manifest() tdriveplugin.Manifest {
	return tdriveplugin.Manifest{
		ID:               "hello",
		Name:             "Hello",
		Description:      "A minimal tdrive plugin.",
		Version:          "0.1.0",
		SDKVersion:       "0.1",
		APIVersion:       1,
		Author:           "tdrive",
		License:          "MIT",
		RepositoryURL:    "https://github.com/dibin666/tdrive",
		DocumentationURL: "https://github.com/dibin666/tdrive/blob/main/docs/plugins.md",
		Entrypoint:       "./cmd/plugin-hello",
		Capabilities:     []string{"events", "http"},
		Events:           []string{"tree"},
		Routes:           []tdriveplugin.RouteSpec{{Path: "/", Methods: []string{"GET"}, UI: true}},
	}
}

func (plugin *helloPlugin) Initialize(_ context.Context, host tdriveplugin.Host) error {
	plugin.host = host
	return nil
}

func (plugin *helloPlugin) OnEvent(ctx context.Context, event tdriveplugin.Event) {
	var settings map[string]any
	_ = plugin.host.Call(ctx, "settings.get", nil, &settings)
	_ = event
}

func (plugin *helloPlugin) HandleHTTP(_ context.Context, _ tdriveplugin.HTTPRequest) (tdriveplugin.HTTPResponse, error) {
	return tdriveplugin.HTTPResponse{
		Status: 200,
		Headers: map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:    []byte("tdrive plugin is running\n"),
	}, nil
}

func main() {
	tdriveplugin.Serve(&helloPlugin{})
}
