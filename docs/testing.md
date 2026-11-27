# Test strategy

The compatibility baseline is evidence about one immutable Stalwart image and
one reviewed server configuration. A version string alone is not sufficient.

## Unit and contract tests

Run for every source revision. They validate typed operation registration,
scope and surface separation, write safety requirements, origin restrictions,
normalization, and error handling. Provider responses use small synthetic
fixtures; no mailbox content or credentials enter the repository.

## Provider integration tests

Run once for each Stalwart image digest and build configuration. A disposable
provider is populated with synthetic accounts, messages, events, contacts,
files, and Sieve rules. The suite captures normalized capabilities,
permissions, management objects, limits, and activation classes, then verifies
the typed operations against them.

Changing the provider digest, configuration contract, or tool source revision
creates a new test identity. Retrying an unchanged revision reuses the recorded
result rather than starting another full suite.

## Runtime canary

Run after delivery through the real ConnecTool and ToolHive path with a
dedicated ServiceInstance. Verify the named cluster, consumer identity,
invocation audience, scope, PublicEdge DNS/TLS path, provider acceptance, and
read-back. A submitted message or scheduling operation is not evidence of
remote delivery or human acknowledgement.

Canary output contains only stable references, counts, state hashes, image and
contract digests, and pass/fail conditions. It excludes credentials, message
content, subjects, recipients, contact data, event descriptions, and file
names.

Before cutover, run every read probe against both the current tenant-mcp tool
and this runtime for the same opaque binding. Compare normalized IDs, state,
counts and truncation flags, not private content. Exercise mail submission and
calendar response only with dedicated synthetic fixtures and explicit preview
plans. Verify provider read-back through both paths. Any missing legacy tool,
scope, authorization flow or normalized field blocks cutover and leaves the
current route unchanged.

## Release gate

An artifact is promotable only when:

1. unit and contract tests pass for its source revision;
2. the provider baseline passes for the named Stalwart image digest;
3. the image and contract digest are signed and immutable;
4. agent-gitops pins both digests; and
5. the runtime canary succeeds through the real consumer path.
