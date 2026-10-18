# Workflow scopes and logical lineage

- **Scope-body identity:** Resolve the exact full `gc.step_ref` within its
  workflow root before unique own-reference suffixes. `gc.scope_ref` names
  enclosing membership and only identifies a legacy body when its own reference
  is absent. Only body-tagged or roleless legacy scope beads qualify; ambiguous
  matches fail closed. Store order and open/closed status do not establish identity.
- Nested Ralph iterations retain their nearest loop's scope and counters when
  an enclosing body is namespaced. Instantiate logical lineage from the complete
  recipe ID mapping before activating work, including forward control references.
- **Scope-abort ordering:** Order every readiness-blocking dependency by skipping
  dependents before prerequisites, so each
  individual close leaves no newly ready work in the aborted scope. Preserve the
  failed subject's recovery scope-check, settle the body and driving control last,
  and retain dependency edges, forced skip outcomes, and genuine-cycle refusal.
