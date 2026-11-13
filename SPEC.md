# Shipkeeper specification

> App-Store reconciler and live pod/error sanitizer in the clearedstack namespace that replaces Argo CD and CasaOS using native Kubernetes Server-Side Apply with zero custom CRDs.

**Status: design.** This is a specification. It describes how Shipkeeper is meant to work, not something that exists yet.

The same text is published at [clearedstack.com/platform/shipkeeper/](https://clearedstack.com/platform/shipkeeper/). To change the specification, change it here and say why.

## Overview & Role

Shipkeeper (`v0.1.0-draft`, owned by the Shipkeeper repository) is a clean-room MIT Go microservice running in the `clearedstack` control-plane namespace. It combines GitOps deployment reconciliation with an App Store lifecycle controller—replacing both Argo CD and CasaOS with a single binary that speaks native Kubernetes **Server-Side Apply** and introduces **zero custom CRDs**.

Shipkeeper watches `app.yaml` manifests in Plainforge and signed digests in Lazarette, reconciles all 17 platform workloads across `clearedstack` (7 control-plane pods) and `cs-workers` (10 deterministic worker pods), and streams sanitized pod health and error codes to Muster for display on `https://clearedstack.com/platform/#registry` and `https://clearedstack.com/platform/operations/`.

## Runtime & Resource Budget

| Property | Value |
| --- | --- |
| **Service / Repo** | Shipkeeper (`shipkeeper`) |
| **Namespace** | `clearedstack` (`control-plane`) |
| **Version** | `v0.1.0-draft` |
| **RAM Budget (Requested)** | `48 MiB` |
| **Priority Class** | High (Control Plane — protected from eviction) |
| **Reconciliation Mechanism** | Native Kubernetes Server-Side Apply (zero custom CRDs) |
| **Sub-Tools** | None (`[]`) |

## Interfaces & Data Contracts

Shipkeeper bridges Git state, OCI digests, Kubernetes workloads, and public Portal telemetry:

- **Desired State (Plainforge + Lazarette):** Reads `app.yaml` definitions from Plainforge and verifies corresponding `sha256` digests in Lazarette (up to 5 retained versions per app).
- **Cluster Reconciliation (Kubernetes API):** Applies `Deployment`, `Service`, and `NetworkPolicy` manifests via Server-Side Apply across `clearedstack` and `cs-workers`.
- **1-Click App Store Actions (`https://clearedstack.com/platform/#registry` via Muster):**
  - **Install:** Applies an app's manifests into its target namespace for the first time.
  - **Upgrade:** Rolls the Deployment forward to the latest signed digest built by Keelworks.
  - **Pause:** Scales the workload down cleanly without deleting its configuration, PVC, or queue state.
  - **Rollback:** Switches `app.yaml` and the live Deployment back to one of the previous 5 signed digests retained in Lazarette.
- **Sanitized Telemetry (Shipkeeper -> Muster):** Watches Kubernetes `Pod`, `Deployment`, and `Event` objects, strips internal metadata, and emits normalized health states (`healthy`, `idle`, `degraded`, `paused`) and standardized error codes (`E_TIMEOUT`, `E_PARSE`, `E_NEEDS_SUBTOOL`).

## Sub-Tools & Failure Modes

Shipkeeper is a control-plane reconciler and does not host `tools/<case>/` deterministic sub-tools (`subTools: []`).

| Failure Mode | Trigger | System Behavior & Recovery |
| --- | --- | --- |
| **Pod crash or failed health check** | New rollout fails readiness or exits non-zero | Surfaces as `P0 · Critical` (`--danger`) on Command and `https://clearedstack.com/platform/#registry` with 1-click **Rollback** to a prior Lazarette digest. |
| **Server-Side Apply conflict** | Manual out-of-band edit collides with Shipkeeper field ownership | Reconciliation logs the conflict cleanly without silently overwriting unrelated cluster state. |
| **Missing or unsigned digest** | `app.yaml` references a digest absent from Lazarette | Deployment rollout is blocked; Shipkeeper preserves the currently running healthy replica set. |
| **Hub API unreachable** | Muster restarts during a status update | Shipkeeper continues reconciling Kubernetes workloads independently and retries telemetry delivery once Muster recovers. |

## Security & Clean-Room Boundaries

- **Strict topology sanitization:** Before forwarding any pod status or Kubernetes event to Muster (and ultimately `pub.*` views), Shipkeeper strips all internal cluster IP addresses, node names, pod hostnames, and internal cluster DNS names. Public visitors see only sanitized health states and standardized error codes.
- **No public Ingress:** Shipkeeper is strictly cluster-private; owner lifecycle actions are authenticated and routed through Muster.
- **Least-privilege RBAC & non-root:** Runs as an unprivileged non-root user with dropped Linux capabilities and scoped Kubernetes RBAC limited to managing platform workloads in `clearedstack` and `cs-workers`.

## Related pages

- [Cluster architecture](https://clearedstack.com/platform/cluster-architecture/)
- [Operating it: status views, failures and backups](https://clearedstack.com/platform/operations/)
