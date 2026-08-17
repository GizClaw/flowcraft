# Declaration-layer plugin example

This directory is a complete zero-code plugin: a strictly decoded
`plugin.yaml` and one `layer` artifact. It registers no factories and
spawns no process; the loader merges its layer into the deployment
document with `deploy.LoadLayers`.

Load it with the plugin shell:

```go
set, err := plugin.NewLoader().Load(ctx, plugin.PluginsConfig{
    Dirs:    []string{"./example/layer"},
    Enabled: []string{"acme.hello-layer"},
})
```

`set.Layers` then contains the merged fragment (priority 100), and
`set.Apply` registers nothing — the plugin is pure configuration.

The loader validates the layer path (containment, size cap, strict
decode) and the explicit-enable whitelist: a misspelled `enabled` entry
fails the load instead of silently enabling nothing.

Run the shipped check:

```sh
go test ./example/layer
```
