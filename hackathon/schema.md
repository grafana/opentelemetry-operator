# Schema Design (complete)

See `instrumentation-v2alpha1-example.yaml` for a fully annotated working example.

## Key decisions

**Scope:** Cluster-scoped CR. Namespace is a selector dimension, not a CR scope.

**Multiple CRs:** Supported. Each pod/container is evaluated against all CRs. Highest `priority` field wins; creation timestamp breaks ties. Emit warning on conflict. No merge semantics.

**Rules:** Array evaluated per `(pod, container)` pair, first-match wins within the winning CR. No match = no instrumentation (safe default).

**No language-specific selection:** Language detection is runtime (injector). If you need different config per language in a multi-language pod, use `selector.containerNames` to target specific containers with separate rules.

**Image config is top-level (not per-rule):** You want all workloads in a CR to use the same agent versions — mixing agent versions across rules within one CR creates confusion and upgrade headaches. If you need different agent versions for different environments (e.g. canary a new Java agent in staging), create a separate CR with higher priority and a namespace selector.

**Per-language agent images:** `spec.java/nodejs/python/dotnet` are optional plain strings that add a per-language init container. When set, the operator injects an env var telling the injector binary where that language's agent lives.

**Config mechanisms:** Two mechanisms per rule (use one or the other):
1. **Env vars** — `corev1.EnvVar` slice (supports `valueFrom.secretKeyRef` etc.). The standard approach when no declarative config is needed.
2. **Declarative config** — inline OTel declarative config (file_format: "1.0"). Operator mounts as ConfigMap, sets `OTEL_CONFIG_FILE`. **Important: SDKs ignore `OTEL_*` env vars when a config file is present**, so all SDK configuration must live in the config document. Env vars can still be set alongside declarativeConfig for non-SDK purposes (e.g. injector vars, secrets injected via `valueFrom` and referenced as `${ENV_VAR}` in the config).

**Conflict-aware instrumentation:** `config.mode` is a tri-state enum (`install`, `skip`, `install_unless_conflict`). Default is `install_unless_conflict` — the injector detects existing manual instrumentation (e.g. Python `sitecustomize`, Node.js SDK imports) and backs off if found. `install` forces instrumentation ("steamroll"), `skip` disables it (opt-out). This replaces the previous `disabled: bool`. Can be set at `spec.defaults.mode` (CR-wide default) and overridden per rule at `config.mode`.

## Schema shape

```
InstrumentationSpec
  priority                int
  injector                string           # injector binary image (libotelinject.so + otelinject.conf)
  java                    string           # Java agent image (optional)
  nodejs                  string           # Node.js agent image (optional)
  python                  string           # Python agent image (optional)
  dotnet                  string           # .NET agent image (optional)
  defaults                                 # CR-wide defaults (overridable per rule)
    mode                  InstrumentationMode  # install | skip | install_unless_conflict (default)
  rules                   []Rule
    name                  string           # optional, for debuggability
    selector
      namespaces          []string         # empty = all namespaces
      podLabels           map[string]string  # AND semantics, empty = all pods
      containerNames      []string         # empty = all containers
    config
      mode                InstrumentationMode  # overrides spec.defaults.mode
      env                 []corev1.EnvVar
      declarativeConfig   *DeclarativeConfig  # inline OTel declarative config
```

## Known gaps (future work)

- **Rollback race condition** — the rollback controller sets `status.instrumentedWorkloads[].rollback` and triggers a pod bounce, but the webhook's `shouldSkipForRollback()` reads the status via the informer cache. If the new pod's admission request arrives before the status update propagates, it gets injected again, defeating the rollback. Fix: either ensure status write completes before bounce, or use a synchronous mechanism (e.g. annotation on the Deployment).
- **Namespace label selectors** — currently only exact namespace name matching; selecting namespaces by label (like NetworkPolicy's `namespaceSelector`) is a v2 enhancement
- **TLS / volume mounts** — no mechanism to mount cert files (e.g. mTLS certs for collector communication) into instrumented containers; workaround is user-managed volumes. Separate from image volumes (which replace the init container pattern for agent binaries)
- **Annotation-based opt-out** — `config.mode: skip` on a rule handles the common case; pod-level annotation opt-out can be added later if needed
- **Instrumentation removal on rule/CR changes** — when a workload stops matching any rule (selector change, rule deletion, CR deletion), existing pods keep running with `LD_PRELOAD` set until manually restarted. The state DB (`status.instrumentedWorkloads[]`) has the inventory to detect this: on reconcile, re-evaluate each inventory entry against current rules and bounce workloads that no longer match. For CR deletion, a finalizer would bounce affected workloads before allowing garbage collection. **Cross-CR handoff:** before bouncing, check whether any other CR would still inject the workload (reuse the webhook's "evaluate all CRs" logic). If another CR claims it, just remove from inventory without bouncing — avoids unnecessary restarts when a workload moves between CRs.
