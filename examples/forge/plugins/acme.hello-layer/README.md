# acme.hello-layer

The forge declaration-layer demo plugin. It contributes one
`event.Bus` resource (`greeting`) with zero code: no process, no
factory registration.

Reference it from a workspace `deploy.yaml`:

```yaml
plugins:
  dirs: [./plugins]
  enabled: [acme.hello-layer]
```

Forge loads the section, merges the plugin layer over the deployment
document, and builds the runtime; `forge workspace inspect` and the
`test`/`tui` commands see the merged document.
