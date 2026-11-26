# Repository instructions

- Keep provider operations typed; never expose arbitrary JMAP methods, DAV
  targets, management objects, command arguments, or credentials.
- Separate user, organization-operator, and platform-administrator tools.
- Require explicit scopes and resource ownership for every operation.
- Material writes use preview/apply, expected state, idempotency, and read-back.
- Store protocol contracts and compatibility fixtures here. GitOps stores only
  immutable artifact and contract digests plus runtime policy references.
- Build each source revision once and reuse its immutable image digest.
- Every pull request closes an Issue in the 8iops Agent Runtime Project.
