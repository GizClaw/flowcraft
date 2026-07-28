# inference provider conformance

`inferencetest` contains reusable black-box contracts for provider packages.
It complements, rather than replaces, tests beside each inference DTO.

Use:

- `RunCompiler` for canonical field ledger and provider-native wire coverage.
- `RunUnary` for generic unary drivers and `RunGenerateUnary` for Generate.
- `RunGenerateCompileParity` for shared, shape-aware unary/stream Generate compilation.
- `RunGenerateStream` for finite Generate stream aggregation and metadata.
- `RunTranscriptionSession` for duplex STT session lifecycle.
- `RunRealtimeSession` for session opening and per-input compiler coverage.
- `Counter` for race-safe compiler, transport, and lifecycle probes.

Provider-specific tests must still inspect the exact native wire payload. The
shared suites verify the cross-provider contract and deliberately do not encode
one provider's request schema.

Generic compiler and unary suites require a `Snapshot` callback that returns an
owned representation of the request, normally its canonical `Clone()` result.
Session suites use the canonical clone methods directly.

```go
inferencetest.RunGenerateUnary(t, inferencetest.GenerateUnarySuite{
    Model:          model,
    Request:        validRequest,
    Driver:         driver,
    TransportCalls: transportCalls.Load,
})
```
