# Hackathon 16 — Composite SDK Injection

## Goal

Replace per-language auto-instrumentation with a single composite SDK image that bundles all language agents and an injector binary. The operator injects this image via init container and the injector handles language detection and agent setup at runtime.

## Branch

- [hackathon-16-composite-sdk-injection](https://github.com/grafana/opentelemetry-operator/tree/hackathon-16-composite-sdk-injection) on `grafana/opentelemetry-operator`
- [hackathon-16-mode-support](https://github.com/grafana/opentelemetry-injector/tree/hackathon-16-mode-support) in `grafana/opentelemetry-injector`

## Design docs

| Doc | Content |
|-----|---------|
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
