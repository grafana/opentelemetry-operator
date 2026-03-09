# E2E Test Fixes — Status as of 2026-03-09

## Fixed

1. **`matchesNamespace` bug**: The v2alpha1 Instrumentation CRD is cluster-scoped, so `inst.Namespace`
   is always empty. The previous fix (scoping catch-all to CR namespace) broke all catch-all rules.
   Restored catch-all to match all non-system namespaces via `isSystemNamespace()`.

2. **All assert files updated**: Fixed env var order, added `kube-api-access` volumeMount (chainsaw
   requires exact array lengths), removed `volumes:` section from assertions.

3. **`concurrent: false`** added to all injector chainsaw-test.yaml files. Cluster-scoped CRs with
   catch-all rules cause cross-contamination when tests run in parallel.

4. **Image placeholders**: All test files use `{{INJECTOR_IMG}}`, `{{INJECTOR_JAVA_IMG}}`,
   `{{INJECTOR_NODEJS_IMG}}` — substituted at `make prepare-e2e` time.

## Test results (sequential, 2026-03-09)

- `injector-basic`: PASS
- `injector-mode`: PASS
- `injector-mode-conflict`: PASS
- `injector-disabled-rule`: PASS
- `injector-pod-label-selector`: PASS
- `injector-declarative-config`: PASS
- `injector-java`: PASS
- `injector-nodejs`: PASS
- `injector-namespace-selector`: PASS
- `injector-rollback`: **FAIL** (step-03 timeout — see gap below)

## Known gap: injector-rollback step-03

Steps 01 (verify injection) and 02 (detect rollback + CrashLoopBackOff) pass.
Step 03 (`verify-new-pods-skip-injection`) times out after 60s — new pods after the
bounce still have `LD_PRELOAD` injected.

**Root cause**: The rollback controller sets `status.instrumentedWorkloads[].rollback`
and triggers a pod bounce via `restartedAt` annotation. But `shouldSkipForRollback()`
in the webhook checks the status before the new pod is admitted. Likely a race: the
status update hasn't propagated to the informer cache when the new pod's admission
request arrives.

**Fix needed**: Either ensure the status write completes before the bounce, or use a
different mechanism (e.g., annotation on the Deployment) that the webhook can check
synchronously.
