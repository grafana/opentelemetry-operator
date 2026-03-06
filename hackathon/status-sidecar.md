# Instrumentation Status: Language Detection

## Problem: the operator is blind after injection

When the operator injects instrumentation into a pod, it doesn't know what language the app is. Language detection happens at **runtime** inside the container (the injector binary inspects the process). The operator only knows it injected "something" -- not whether it was Java, Python, Node.js, etc.

This matters for two features:

1. **Selective pod bouncing** -- when someone updates the Java agent image in the config, only restart Java workloads, not Python ones. Without language info, we'd restart everything (wasteful and disruptive).
2. **Observability dashboards** -- "how many pods are instrumented per language?", "how often do conflicts occur?" -- needs per-pod language data.

## Options

Three approaches for getting language info back to the operator, each with different trade-offs.

### Option A: Collector-side language metric

The instrumented app already sends `telemetry.sdk.language` as a resource attribute with every metric/span/log. The collector processes all this telemetry and can expose a per-workload language gauge on its own `/metrics` endpoint. The operator scrapes the collector directly -- no external Prometheus dependency, no pod-spec changes.

**Data flow:**

```
App (instrumented)
  |  sends telemetry with resource attributes:
  |    telemetry.sdk.language = "java"
  |    k8s.namespace.name, k8s.deployment.name (set by operator)
  v
Collector
  |  processor/connector maintains pod -> language map
  |  exposes on its own /metrics endpoint:
  |    otel_observed_sdk_language{
  |      k8s_namespace_name="production",
  |      k8s_deployment_name="checkout-service",
  |      telemetry_sdk_language="java"
  |    } 1
  v
Operator scrapes collector /metrics directly
```

**Operator integration:**

```
Instrumentation CR spec:
  collectorMetricsEndpoint: "http://otel-collector.monitoring:8888/metrics"

On CR spec change:
  1. Scrape collector metrics endpoint
  2. Parse otel_observed_sdk_language series -> build workload -> language map
  3. Diff: which spec fields changed? (e.g. spec.java image)
  4. Bounce only workloads where language matches changed fields
  5. Unknown language (no metric yet) -> bounce (safe default)
```

**Collector component:** A processor or connector that observes `telemetry.sdk.language` from incoming resource attributes and emits a gauge on the collector's internal metrics. This is a small, stateless component -- it just maintains a map of recently-seen `(namespace, deployment, language)` tuples.

**Pros:**
- Zero pod-spec changes -- no sidecar, no shareProcessNamespace, no volumes
- Zero injector changes -- uses data the SDK already sends
- Collector is always cluster-local -- no auth, no external dependency on cloud Prometheus
- Graceful degradation -- without config, bounce all (still works, just less selective)
- Observability dashboards can scrape the same collector metric
- Optional -- `collectorMetricsEndpoint` is opt-in
- Follows the natural data flow -- language info already passes through the collector

**Cons:**
- Latency -- metric appears after first telemetry export from the app (typically 10-60s). Freshly started pods have no data yet.
- Requires a collector component (processor/connector) to extract and expose the metric
- Collector must be reachable from the operator (usually trivial within a cluster)
- If the collector restarts, the in-memory map is lost until pods re-export (stateless, rebuilds quickly)

**Edge cases:**

| Case | Behavior |
|------|----------|
| Pod just started, no telemetry yet | Unknown -> bounce (safe default) |
| Collector restarted | Map rebuilds as pods export; unknown -> bounce |
| `collectorMetricsEndpoint` not configured | Selective bouncing disabled, always bounce all |
| Multi-container pod, different languages | Distinct series per `k8s.container.name` label |
| Collector not managed by this operator | User sets endpoint manually; works the same |

### Option B: Env var + shareProcessNamespace

The injector already detects language internally (it has to, to know which agent to activate). It sets an additional env var like `OTEL_INJECTOR_DETECTED_LANGUAGE=java` in-process. A sidecar reads this via `/proc/1/environ` using `shareProcessNamespace`.

**Data flow:**

```
Pod spec:
  shareProcessNamespace: true    <- operator adds this

  App container:
    injector runs via LD_PRELOAD
    sets OTEL_INJECTOR_DETECTED_LANGUAGE=java in-process
    (no file writes, no new mounts)

  Sidecar container:
    reads /proc/1/environ -> finds OTEL_INJECTOR_DETECTED_LANGUAGE=java
    serves GET /metrics -> otel_injector_info{language="java"} 1
```

**Pros:**
- Single source of truth -- reads the injector's own detection result, not a second detector
- No writes from app container -- env var is set in-process, not written to a volume
- App container security context completely unchanged -- no new mounts, no new write paths
- Real-time -- available immediately after process starts (no export interval delay)

**Cons:**
- `shareProcessNamespace` lets all containers see each other's processes and send signals
- Adds a sidecar container (small: idle HTTP server, ~1m CPU / 8Mi memory)
- Requires injector change to set `OTEL_INJECTOR_DETECTED_LANGUAGE` -- needs injector SIG acceptance for upstream
- Env var is only readable via /proc while the process is running

**Security notes:**
- The sidecar is operator-controlled -- can run with `allowPrivilegeEscalation: false`, drop all capabilities, read-only root FS
- `shareProcessNamespace` is GA since k8s 1.17, allowed by all PSA profiles including `restricted`
- The app container gains no new capabilities -- only the sidecar can see processes

### Option C: Status file on shared volume

The injector writes a status file to a shared emptyDir volume. A sidecar reads it and serves metrics over HTTP.

**Data flow:**

```
Pod:
  App container:
    injector writes /otel-status/status.json
    (language, mode, conflicts)

  Shared emptyDir:
    /otel-status/status.json

  Sidecar container:
    reads status.json
    serves GET /metrics -> otel_injector_info{language="java"} 1
```

**Prometheus exposition format:**

```
otel_injector_info{language="java",mode="install",conflict="false"} 1
otel_injector_injection_result{result="success"} 1
```

**Pros:**
- Richest data -- injector can write arbitrary status (language, mode, conflicts, agent version, config hash)
- Real-time -- available immediately after LD_PRELOAD runs, before main()
- No shareProcessNamespace required
- Extensible without protocol changes

**Cons:**
- App container writes to a shared volume it didn't before -- security concern for read-only deployments
- "The injector is a part of the application process, it's the very definition of malicious activity" (Nikola's feedback)
- Adds a sidecar container and an emptyDir volume mount to the app container
- Requires injector change to write status file -- needs injector SIG acceptance for upstream

**Mitigations:**
- `medium: Memory` (tmpfs) -- no disk I/O, no persistence, explicit `sizeLimit: 1Mi`
- emptyDir is allowed under PSA `restricted` profile
- The operator controls both sides (volume, mount, sidecar)

## Comparison

| | A: Collector metric | B: Env var + shareProcessNamespace | C: Status file |
|---|---|---|---|
| Pod-spec changes | None | shareProcessNamespace + sidecar | emptyDir + sidecar |
| App container changes | None | None | New volume mount (writable) |
| Injector changes | None | Set env var | Write file |
| Collector changes | Processor/connector to expose metric | None | None |
| Data availability | After first export (10-60s) | Immediate | Immediate |
| Data richness | telemetry.sdk.language only | Language only (extensible via more env vars) | Arbitrary (JSON) |
| External dependency | Collector reachable from operator | None | None |
| Selective bouncing | Yes (with fallback) | Yes | Yes |
| Observability dashboards | Scrape same collector metric | Needs PodMonitor scraping sidecar | Needs PodMonitor scraping sidecar |
| Security posture | No change | shareProcessNamespace (containers see each other's processes) | App writes to shared volume |
| Injector SIG acceptance | No changes needed | Needs env var addition | Needs file write addition |

## Rejected approaches

### Options B and C: Injector reports language back

Both Option B (env var + shareProcessNamespace) and Option C (status file) assume the injector binary detects the language and can report it back. In the composite SDK injection model, **the injector doesn't know the language**. It's language-agnostic — it sets up LD_PRELOAD and the composite SDK handles everything. Since the injector doesn't detect language, it can't report it, which invalidates both approaches.

## Conclusion: scoped out

After design exploration and discussion with Nikola, we're scoping selective pod bouncing out of the hackathon.

**Why:** All three approaches have fundamental gaps for reliable automation:

- **Option A (collector metric)** is the least invasive but depends on a signal that may never arrive — if a service has no traffic, hasn't exported telemetry yet, or the collector pipeline is misconfigured, the operator has no language data. You can't build reliable bounce automation on a signal with no delivery guarantee. Even with "unknown → skip" as a safe default, coverage is only as good as telemetry flow.
- **Options B and C (sidecar-based)** require injector changes that don't fit the composite SDK model, where the injector is language-agnostic.
- **Daemonset approach** (as used by Odigos, Datadog, Beyla) would give reliable process-level language detection but is a major architectural addition — the OTel operator currently has zero node-level components. This needs broader community discussion, not a hackathon.

**Key insight from Nikola:** The real problem isn't language detection — it's "which pods are eligible for bouncing." Language is a subset. A daemonset can inspect processes to determine language, command-line arguments, and eligibility, solving the problem reliably. Without one, any approach has blind spots.

**Learnings preserved:** The three options and their trade-offs are documented above for future reference if the community decides to tackle this.
