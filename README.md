# Stalwart Agent Tools

Typed Stalwart tools and provider API conformance tests for the RE8CH Agent
runtime. Source implementation and provider compatibility baselines live here;
deployment desired state belongs to `8iops/agent-gitops`.

## Ownership boundary

- This repository owns typed provider operations, product scopes, protocol
  compatibility, normalized errors, and conformance fixtures.
- `re8ch/service-backend` owns tenant grants, ServiceInstances, opaque user
  bindings, invocation authorization, and neutral usage events.
- ConnecTool and ToolHive own MCP aggregation and runtime isolation. They do
  not implement Stalwart protocol semantics.
- `8iops/agent-gitops` owns only immutable image and contract digests, ToolHive
  publication, policy references, Secret references, probes, and rollback.
- Stalwart remains an Atlantic-owned service consumed through reviewed
  PublicEdge endpoints. AmongCluster is not a Kubernetes or credential proxy.

The initial machine-readable boundary is [`contracts/provider.json`](contracts/provider.json).

## Development

```sh
gofmt -w .
go vet ./...
go test ./...
go run ./cmd/contractlint
```

Add capabilities through [`contracts/tools.json`](contracts/tools.json), not by
accepting method names or provider URLs from callers. The catalog validator
requires every write to declare preview, idempotency, state-precondition, and
read-back behavior. See [`CONTRIBUTING.md`](CONTRIBUTING.md) and
[`docs/testing.md`](docs/testing.md).

Initial delivery is tracked in
[8iops/stalwart-agent-tools#1](https://github.com/8iops/stalwart-agent-tools/issues/1).
