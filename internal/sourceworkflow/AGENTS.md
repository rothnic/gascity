# Source workflow readiness

Source prerequisite propagation belongs here so sling and formula cook share
the store's readiness rules.

- Use `beads.IsReadyBlockingDependencyType`, `beads.DependencySatisfied`, and
  `beads.ReadinessWorkOutcome`. Preserve dependency types.
- Pass entry dependencies through `molecule.Options.ExternalDeps` before
  instantiation. Entry work must never appear ready ahead of its prerequisites.
- Resolve prerequisites in both the source and workflow stores. Unsatisfied
  prerequisites require the same original store handle; matching IDs in separate
  stores do not prove identity. Refuse unknown sources, members, and targets
  explicitly. Never discard a foreign dependency.
- Use `beads.HandlesFor(store).Live` for the creation-time snapshot, including
  convoy membership. A primed cache can omit prerequisites added before launch.
- Keep existing source/root locks and singleton checks. Reuse an existing root
  before computing new entry dependencies, so repeated attachment cannot add a
  dependency on itself.
- Depend on source prerequisites rather than the source bead: cook/attach can
  already make the source wait for its workflow root.
- Scheduling readiness does not establish source integration or code acceptance.

The owning behavior regressions are `TestSourceReadiness*` in `internal/sling`
and `TestSourceReadinessFormulaCookAttach` in `cmd/gc`.
