# Claude Code Assistant Guidelines

## Go Code Style

- Follow the standard Go code style and conventions. Use `gofmt` for formatting and adhere to idiomatic Go practices.
- Follow best practices from the [Effective Go](https://go.dev/doc/effective_go) guide:

### Naming Conventions
- Use **MixedCaps** or **mixedCaps** rather than underscores for multi-word names
- Package names should be short, lowercase, single-word names
- Getters don't use "Get" prefix (use `obj.Name()` not `obj.GetName()`)
- Interface names use "-er" suffix for single-method interfaces (e.g., `Reader`, `Writer`)

### Formatting
- Use `gofmt` for consistent formatting (tabs for indentation, spaces for alignment)
- Line length: no strict limit, but keep lines reasonable
- Group related declarations together

### Error Handling
- Return errors as the last return value
- Check errors immediately after the call
- Provide context with `fmt.Errorf` and error wrapping

### Logging
- Use `ctrl.Log` for structured logging
- Keep log fields consistent and meaningful
- Avoid logging sensitive data

### Documentation
- Every exported name should have a doc comment
- Start comments with the name being described
- Use complete sentences

### Concurrency
- Share memory by communicating; don't communicate by sharing memory
- Use channels to orchestrate goroutines
- Always handle goroutine cleanup and cancellation properly

### Project Structure
- Keep packages focused and cohesive
- Avoid circular dependencies
- Place tests in `*_test.go` files

### Headers and Licenses

- Do not include license headers in source files.

## Documentation

Prefer placing documentation in the `docs/` directory. It is split by audience,
and that split is the point: a reader must be able to tell from the path whether
a page is written for them.

**User-facing** - someone running WVA, not changing it:

1. **Well-lit paths** - one scenario, in `docs/well-lit-paths/<scenario>/`: what
   it buys, what it costs, when not to take it, and which suites and benchmark
   scenario back it. Paths link down into guides; they never repeat the steps.
2. **Guides** - one task, from nothing to working, in `docs/guides/<task>/`.
   Bash blocks between `guide:` markers are generated from `guide.yaml`; edit the
   YAML and run `make guides-render`, never the block.
3. **Reference** - what an operator sets and reads, in `docs/reference/`:
   configuration, scaling policy, metrics, troubleshooting.
4. **Concepts** - how WVA decides, in an operator's terms, in `docs/concepts/`.

**Developer-facing** - contributors and maintainers:

5. **Developer documentation** - development setup and workflow, and component
   internals written in package paths and struct fields, in
   `docs/developer-guide/`.
6. **Proposals** - design notes for work that is not built yet, or built and
   still moving, in `docs/proposals/`.
7. **Agent plans and specs** - in `docs/plans/<area>/`, where `<area>` is the
   part of the project (e.g. `engine`, `installation`, `monitoring`). Superpowers
   specs go here too, beside the plan that implements them.

Every page belongs to exactly one of these. A page useful to both audiences is
user-facing, and links down to the developer page rather than absorbing it.


## Kustomize / Config File Naming

All files under `config/` follow the `(<app>-)?<kind>.yaml` pattern:

- **`<kind>`** is the Kubernetes kind as a single lowercase word — no hyphens.
  - `ConfigMap` → `configmap`, `ClusterRole` → `clusterrole`, `ClusterRoleBinding` → `clusterrolebinding`, `RoleBinding` → `rolebinding`, `ServiceAccount` → `serviceaccount`, `ServiceMonitor` → `servicemonitor`, `CustomResourceDefinition` → `customresourcedefinition`
- **`<app>`** is an optional kebab-case prefix identifying the component or logical scope of the resource.
- The app prefix comes **first**, the kind suffix comes **last**: e.g., `saturation-scaling-configmap.yaml`, `epp-metrics-serviceaccount.yaml`, `manager-clusterrole.yaml`.
- `kustomization.yaml` and other kustomize-internal files are exempt.
- File names use hyphens (`-`), never underscores.

## E2E Testing

- use make targets for running e2e tests (e.g., `make test-e2e-smoke` or `make test-e2e-full`) and document the process in `docs/developer-guide/testing.md`
- use `make test` for unit tests
- **Never use images from docker.io in e2e tests.** All container images must use fully-qualified registry paths (e.g., `registry.k8s.io/`, `quay.io/`, or a private registry). Do not rely on Docker Hub as a default registry.

## Deprecation

- the helm chart has been removed. Do not re-introduce it or add helm chart features.
