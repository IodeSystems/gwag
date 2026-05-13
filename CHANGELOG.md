# Changelog

Notable changes to gwag, conventional-commits-derived but human-edited.
One section per release; `Unreleased` at the top while main moves.

The version contract is in [`docs/stability.md`](./docs/stability.md):
SemVer over the symbols marked `// Stability: stable`, additive
changes on MINOR, drops on MAJOR.

## Unreleased

### Changed
- Wire-level identifiers renamed `go-api-gateway` → `gwag` across
  JetStream KV bucket names (`gwag-{registry,peers,stable,
  deprecated,mcp-config}`), the default NATS cluster name
  (`gwag`), the MCP server-info string returned by `MCPHandler`,
  and the UI's `localStorage` keys (`gwag:admin-token`,
  `gwag:admin-token-changed`). Pre-1.0 cleanup so the project
  ships with one consistent identifier.

### Migration
- Cluster operators: any existing JetStream data under the old
  bucket names needs to be wiped (or migrated by hand). New
  installs are unaffected.
- Dev installs from any pre-1.0 build: `rm -rf .gw/` and re-login
  via `gwag login`. The persisted admin token under the old data
  dir is harmless but the UI's saved bearer (under the old
  localStorage key) won't be read back.
- Standalone gateways with a custom `ClusterName` are unaffected —
  only the default value changed.
