# Hackathon 16 — Composite SDK Injection

## Goal

Replace per-language auto-instrumentation with a single composite SDK image that bundles all language agents and an injector binary. The operator injects this image via `LD_PRELOAD` + image volumes and the injector handles language detection and agent setup at runtime.

## Status (as of 2026-03-09)

**Working end-to-end:** 9/10 e2e tests pass. The new v2alpha1 Instrumentation CRD, webhook injection, declarative config, mode support, namespace/pod-label selectors, and per-language agent images all work.

### What's done

- v2alpha1 Instrumentation CRD (cluster-scoped, rule-based, priority ordering)
- Webhook pod mutation: `LD_PRELOAD` injection, image volumes, env vars, downward API
- Per-language agent images (Java, Node.js, Python, .NET) via separate image volumes
- Declarative config support (inline OTel config → ConfigMap → `OTEL_CONFIG_FILE`)
- Conflict-aware modes: `install`, `skip`, `install_unless_conflict`
- Namespace selectors, pod label selectors, container name selectors
- Crash-loop rollback controller (detection + bounce works, but see gap below)
- Instrumentation status metrics (`otel_injector_instrumented_workloads` counter)
- E2e tests for all of the above

### Known gaps / TODOs

1. **Rollback race condition** — rollback detection and bounce work (e2e step-02 passes), but new pods after the bounce still get injected because the status update hasn't propagated to the webhook's informer cache. See [e2e-test-fixes.md](e2e-test-fixes.md) and [schema.md](schema.md#known-gaps-future-work).
2. **Tests must run sequentially** — `concurrent: false` is set on all injector tests because the CRD is cluster-scoped and catch-all rules cause cross-contamination in parallel runs.
3. **Namespace label selectors** — only exact name matching, no label-based namespace selection.
4. **Instrumentation removal** — when workloads stop matching rules, existing pods keep `LD_PRELOAD` until manually restarted.
5. **TLS / volume mounts** — no mechanism for cert files in instrumented containers.

### How to pick up work

```bash
git checkout hackathon-16-composite-sdk-injection
make prepare-e2e                    # builds images, starts kind, deploys operator
./bin/chainsaw test ./tests/e2e-instrumentation/injector-*/   # run all injector tests
```

## Branch

- [hackathon-16-composite-sdk-injection](https://github.com/grafana/opentelemetry-operator/tree/hackathon-16-composite-sdk-injection) on `grafana/opentelemetry-operator`
- [hackathon-16-mode-support](https://github.com/grafana/opentelemetry-injector/tree/hackathon-16-mode-support) in `grafana/opentelemetry-injector`

## Design docs

| Doc | Content |
|-----|---------|
| [Operator autoinstrumentation redesign](https://github.com/grafana/internal-docs/blob/main/docs/obi/2026-03-02-operator-autoinstrumentation-redesign.md) | Internal design doc — motivation, architecture, rollout plan |
| [Injection flow](injection-flow.md) | How operator + injector work together, env vars, building from source, e2e testing |
| [Schema](schema.md) | CRD schema design, shape, key decisions, known gaps |
| [Crash-loop recovery](crashloop-recovery.md) | Auto-detection, timing model, rollback flow, schema additions |
| [Instrumentation metrics](instrumentation-metrics.md) | Prometheus metric for per-workload instrumentation status (instrumented, pending_restart, rolled_back, skipped, unmatched) |
| [Status sidecar](status-sidecar.md) | Pod bouncing + operator telemetry via Prometheus-compatible sidecar (superseded by instrumentation metrics) |
| [v1alpha1 comparison](v1alpha1-comparison.md) | What changed vs v1alpha1 and why |
| [Decisions](decisions.md) | Mar 4 sync decisions, future work |
| [CRASHLOOP-RECOVERY-DESIGN.md](../CRASHLOOP-RECOVERY-DESIGN.md) | Full design rationale and research for crash-loop recovery |

## Key files

| File | Purpose |
|------|---------|
| `apis/v2alpha1/instrumentation_types.go` | CRD Go types — edit this to add fields |
| `apis/v2alpha1/groupversion_info.go` | API group/version registration |
| `apis/v2alpha1/zz_generated.deepcopy.go` | Auto-generated — do not edit |
| `config/crd/bases/instrumentation.opentelemetry.io_instrumentations.yaml` | Generated CRD YAML — do not edit |
| `instrumentation-v2alpha1-example.yaml` | Annotated reference example CR |
| `internal/injector/podmutator.go` | CR lookup + PodMutator implementation |
| `internal/injector/inject.go` | Pod mutation logic (init container, env vars, config mount) |
| `internal/injector/controller.go` | Reconciler — ConfigMap lifecycle for declarativeConfig |
| `internal/injector/inject_test.go` | Unit tests |
| `internal/injector/rollback_controller.go` | Crash-loop detection, workload inventory, rollback triggering, metrics update |
| `internal/injector/metrics.go` | Pod classification logic (`computeStatusCounts`, `findRuleForPod`) |
| `apis/v2alpha1/metrics.go` | `InstrumentationMetrics` — OTel observable counter registration and thread-safe measurement store |
| `main.go` | Scheme + mutator + controller registration; meter provider bootstrap |

## After changing CRD types

```bash
make generate   # regenerates zz_generated.deepcopy.go
make manifests  # regenerates CRD YAML
```

Use [kubebuilder markers](https://book.kubebuilder.io/reference/markers) for validation on new fields.
