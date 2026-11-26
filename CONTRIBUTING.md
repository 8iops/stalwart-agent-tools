# Contributing

Each change adds one reviewed capability or one shared protocol invariant.

## Add a capability

1. Add a typed operation to `contracts/tools.json`.
2. Select exactly one surface: `user`, `organization`, or `platform`.
3. Give the operation a product scope; provider permissions remain an internal
   compatibility concern.
4. For a material write, require preview, expected state, idempotency, and
   read-back. Document whether provider acceptance proves submission only or a
   stronger outcome.
5. Implement the operation without accepting arbitrary JMAP methods, DAV URLs,
   management object names, command arguments, or provider credentials.
6. Add unit tests, provider fixtures, and a canary case as appropriate.
7. Update the capability baseline only from a named immutable provider image.

## Pull requests

- One Issue, concern, branch, and pull request form one review unit.
- Branch names include the Issue number.
- Commits are signed and remain small and coherent.
- Build and test a source revision once; reuse its immutable artifact digest.
- Runtime delivery and verification happen in `8iops/agent-gitops` after the
  source artifact is signed.
