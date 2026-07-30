// Package config assembles runnable agents from declarative YAML
// documents: engine kind + resolved dependencies + lifecycle hooks +
// per-call policy, all bound through named, application-supplied
// sources and factories.
//
// The document is PURE YAML end to end — every spec/settings block
// is decoded by yaml.v3 into native Go values or yamlv3.Node; no
// JSON intermediate ever appears. Unknown fields are rejected at
// parse time so typos surface immediately.
//
// # Document shape
//
//	version: v1
//	agents:
//	  researcher:
//	    card: { name: 研究员, description: 深度调研 }
//	    tools: [search, fetch]
//	    engine:
//	      kind: graph                      # looked up in agent.Registry
//	      settings:                        # opaque to the loader; the
//	        graph: graphs/research.yaml    # engine factory validates it
//	    deps:                              # keyed by the factory's DepSpec
//	      llm:   { source: inference.profile, ref: kimi-k2 }
//	      tools: { source: tool.catalog,    ref: default }
//	    before: { type: history, settings: { window: 20 } }
//	    hooks:
//	      - { type: transcript, settings: { store: history } }
//	    after:
//	      - type: discard_on_interrupt     # built-in kind
//	        settings: { reason: barge-in, causes: [user_input] }
//	    policy:
//	      max_revise: 2
//	      artifact_channels: [drafts]
//
// # What the loader does NOT own
//
// Values YAML cannot express — inference registries, tool catalogs,
// hook implementations — arrive as named SOURCES and hook/seed
// factories registered on [Builder] by the application. This package
// never imports sdk/inference, sdk/tool, or any concrete engine; the
// application wires the closures.
package config
