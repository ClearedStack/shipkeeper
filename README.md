<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="brand/logo-dark.svg">
    <img src="brand/logo-light.svg" alt="Shipkeeper" height="56">
  </picture>
</p>

<p align="center">
  <strong>Deploys what was built, keeps it in its intended state and reports its health with cluster details removed.</strong><br>
  Part of <a href="https://clearedstack.com">ClearedStack</a>
</p>

---

> **Status: ready to deploy.** Shipkeeper builds with Go's standard library only, reconciles all seventeen registered ClearedStack components across `clearedstack` and `cs-workers` against Plainforge, Lazarette (including ECDSA P-256 `cosign` SimpleSigning verification), Kubernetes Server-Side Apply, and Muster's shared contract v1 (`POST /v1/health`), replays all 52 saved examples from `ClearedStack/muster/contract/v1/examples/`, and includes hardened Kubernetes manifests in [`deploy/`](deploy). See [What works today](#what-works-today) and [What is missing from the contract and neighbours](#what-is-missing-from-the-contract-and-neighbours).

Shipkeeper is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Shipkeeper is meant to be understood and used without the rest.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Shipkeeper does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component's entry in the ClearedStack naming registry. |
| [`app.yaml`](app.yaml) | Desired-state release manifest for Shipkeeper, updated by Keelworks on signed builds. |
| [`brand/`](brand) | The Shipkeeper logo and mark, for light and dark backgrounds. |
| [`cmd/`](cmd), [`internal/`](internal) | The reconciler, Plainforge/Lazarette/Kubernetes/Muster clients, and test suite. |
| [`.ko.yaml`](.ko.yaml), [`deploy/`](deploy) | `ko` image build settings and hardened Kubernetes manifests (`Namespace`, `ServiceAccount`, `Role`, `RoleBinding`, `NetworkPolicy`, `Deployment`). |

## At a glance

| | |
| --- | --- |
| **Role** | Control-plane service |
| **Formerly** | `cs-store`, then Bosun |
| **Namespace** | `clearedstack` (manages `clearedstack` and `cs-workers`) |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/shipkeeper` |
| **Memory budget** | 48 MiB (`GOMEMLIMIT=40MiB`) |
| **Depends on** | [Plainforge](https://github.com/ClearedStack/plainforge), [Lazarette](https://github.com/ClearedStack/lazarette), [Muster](https://github.com/ClearedStack/muster) |
| **Depended on by** | Nothing else in the platform |

### Boundaries

- Kubernetes API, using Server-Side Apply (`fieldManager=shipkeeper`, `force=false`) across `clearedstack` and `cs-workers`
- Reads `app.yaml` manifests from Plainforge over Git smart HTTP (`/<repo>.git/info/refs?service=git-upload-pack` and `/<repo>.git/git-upload-pack`)
- Verifies pinned `sha256` image digests and ECDSA P-256 `cosign` SimpleSigning signatures in Lazarette over OCI Distribution v2/v1.1 (`HEAD /v2/<image>/manifests/<digest>` and `GET /v2/<image>/referrers/<digest>`, with `sha256-<hex>.sig` tag fallback)
- Sends sanitized health reports (`POST /v1/health`) to Muster as `system:serviceaccount:clearedstack:shipkeeper`, never touching Logbook (PostgreSQL 17) directly

## What works today

1. **In-cluster service mode (`shipkeeper serve`):**
   - Polls all 17 registered platform components (`7` in `clearedstack`, `10` in `cs-workers`) on a fixed interval (`30s` by default, or one pass with `-once`).
   - Fetches each component's `app.yaml` from Plainforge over Git smart HTTP with an in-memory commit-OID cache so unchanged repositories incur no pack upload.
   - Verifies both OCI manifest presence and ECDSA P-256 `cosign` SimpleSigning signature (under `/var/run/secrets/keelworks/cosign/cosign.pub`, re-read per call for live key rotation) in Lazarette before touching the cluster; if missing or unsigned, preserves the currently running healthy replica set and reports `degraded` with `E_NEEDS_SUBTOOL`.
   - Applies a `NetworkPolicy`, `Service`, and `Deployment` with Kubernetes Server-Side Apply (`dryRun=All` across all three first, `force=false` always) so manual edits are never overwritten.
   - Observes existing cluster workloads (including components without `app.yaml` in Plainforge yet, such as the `logbook` `StatefulSet`), strips all node names, pod names, IP addresses, and internal DNS names, and sends a single batched `POST /v1/health` request to Muster.
   - Includes `digest` only once a rollout is `healthy`, triggering Muster's Logbook PostgreSQL 17 transaction to replay paused worker items (`core.pause`) and mark `building` edit proposals (`core.edit_proposal`) as `live`.
   - Buffers pending health reports in memory and retries automatically across Muster restarts or Logbook slowdowns (`503 store_slow`).
   - Re-reads projected Kubernetes `ServiceAccount` bearer tokens from disk on each call so kubelet token rotation works without pod restarts, and serves `GET /healthz` on `:8080` for readiness and liveness probes.
2. **Standalone single-app mode (`shipkeeper -app ...`):**
   - Reads one `app.yaml` and a local digest list, applies to a cluster, and prints the sanitized report JSON to standard output.

### Try it

Needs Go 1.24 or later and nothing else. Standard library only (`go.mod` has zero external dependencies).

```sh
go vet ./...
go test -race ./...                # all unit, integration, cosign, and 52 Muster contract v1 replay tests
go run ./cmd/shipkeeper-demo       # standalone pass against the in-memory cluster
```

Run one full control-plane reconciliation pass against Plainforge, Lazarette, Muster, and a Kubernetes API server:

```sh
go run ./cmd/shipkeeper serve -once \
  -plainforge-url http://plainforge.clearedstack.svc.cluster.local \
  -lazarette-url http://lazarette.clearedstack.svc.cluster.local:5000 \
  -cosign-pub-file /var/run/secrets/keelworks/cosign/cosign.pub \
  -muster-url http://muster.clearedstack.svc.cluster.local:8080
```

### The `app.yaml` read today

`SPEC.md` does not specify the exact schema of `app.yaml`. Shipkeeper and Keelworks share this strict YAML subset:

```yaml
name: assay                  # component slug, and the name of its three objects
namespace: cs-workers        # clearedstack or cs-workers, matching the naming registry
image: assay                 # repository name in Lazarette, with no host
digest: sha256:4e7a...       # 64 hex digits; must exist and verify in Lazarette
replicas: 2                  # 1 to 10
port: 8080                   # the one TCP port the app listens on
memory: 32Mi                 # request and limit
allowFrom:                   # optional: peers allowed to connect; empty means nobody
  - clearedstack/muster
```

Only comments, `key: value`, lists of plain values, and simple quotes are accepted. Anchors, tags, flow collections, block scalars, multiple documents, and escapes are refused with `E_NEEDS_SUBTOOL`.

### Saved examples

| Rules | Saved examples |
| --- | --- |
| Muster shared contract v1 (`POST /v1/health`, `GET /healthz`, all 28 refusals, and all 20 worker/public envelopes) | [`internal/muster/testdata/examples`](internal/muster/testdata/examples): 52 files from `ClearedStack/muster/contract/v1/examples/` |
| Reading `app.yaml`, and the three objects made from it | [`internal/manifest/testdata`](internal/manifest/testdata): 36 cases, `app.yaml` -> `objects.json` or `error.txt` |
| Removing cluster details from text | [`internal/sanitize/testdata/cases.json`](internal/sanitize/testdata/cases.json): 39 cases |
| Health state and the report | [`internal/health/testdata`](internal/health/testdata): 11 cases, `observed.json` -> `report.json` or `error.txt` |
| Single-app reconciliation pass | [`internal/reconcile/testdata`](internal/reconcile/testdata), from [`internal/example/files`](internal/example/files) |
| Every error Shipkeeper can report | [`internal/errs/testdata/catalogue.txt`](internal/errs/testdata/catalogue.txt) |
| Command output | [`cmd/shipkeeper/testdata`](cmd/shipkeeper/testdata) |

## What does not work yet

- **1-click App Store actions (`Install`, `Upgrade`, `Pause`, `Rollback`) from Muster:** `SPEC.md` describes owner-triggered lifecycle commands routed through Muster, including switching back to one of 5 retained digests in Lazarette. Muster's `contract/v1/CONTRACT.md` defines only `POST /v1/health` for `shipkeeper` and explicitly forbids inventing endpoints, fields, or work-item kinds. Initial `Install` and GitOps `Upgrade` happen automatically when `app.yaml` appears or advances in Plainforge; interactive `Pause` and `Rollback` await a contract addition in Muster.
- **`idle` health state from Shipkeeper:** `SPEC.md` lists `idle` among health states, without defining a Kubernetes pod rule for it. In Muster's `CONTRACT.md`, `POST /v1/health` accepts `healthy`, `idle`, `degraded`, and `paused`, while Muster itself derives worker `idle` from lease inactivity in Logbook; Shipkeeper therefore emits `healthy`, `degraded`, and `paused` from Kubernetes rollout state without guessing `idle`.
- **Event streaming vs bounded polling:** Shipkeeper reconciles and observes `Deployment`, `Pod`, and `Event` resources every `SHIPKEEPER_INTERVAL` (`30s`) rather than holding open unbounded Kubernetes watch streams, keeping resident memory comfortably within its `48 MiB` limit.
- **Live cluster validation:** All Kubernetes tests run against `internal/kubetest` (`TestRealAPIServer` is ready to run when pointed at a throwaway cluster, and the `48 MiB` pod limit has not yet been measured on a live `arm64` node).

## What is missing from the contract and neighbours

1. **Muster (`ClearedStack/muster`):**
   - `contract/v1/CONTRACT.md` needs a contract call (or lease/action envelope) if Muster is to dispatch owner-triggered `Install`, `Upgrade`, `Pause`, and `Rollback` commands to Shipkeeper, plus a rule if Shipkeeper (rather than Muster) should ever emit `idle`.
   - Muster, Plainforge, and Lazarette on `main` already allow NetworkPolicy ingress from `shipkeeper` in `clearedstack`.
2. **Plainforge (`ClearedStack/plainforge`) & Component Repositories:**
   - Each managed component repository needs a root `app.yaml` once Shipkeeper is expected to apply its `Deployment`, `Service`, and `NetworkPolicy` (until then, Shipkeeper observes existing workloads read-only without failing).
3. **Bootstrap image digest & `keelworks-cosign` Secret:**
   - At cluster bootstrap, `keelworks-cosign` (`cosign.pub`) is generated in `clearedstack`, and Keelworks/`ko` pushes the signed `linux/arm64` image to Lazarette to replace the `sha256:0000...` bootstrap placeholder in [`deploy/shipkeeper.yaml`](deploy/shipkeeper.yaml) and [`app.yaml`](app.yaml).

## Names

`shipkeeper` is this component's identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component's entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
