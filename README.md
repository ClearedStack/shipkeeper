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

> **Status: design.** The specification is not built. One first milestone is: Shipkeeper can read one `app.yaml`, apply it with server-side apply, and report the app's health with node names and addresses removed. It has been run only against an in-memory stand-in for the Kubernetes API, never against a real cluster. See [What works today](#what-works-today).

Shipkeeper is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Shipkeeper is meant to be understood and used without the rest.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Shipkeeper does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`brand/`](brand) | The Shipkeeper logo and mark, for light and dark backgrounds. |
| [`cmd/`](cmd), [`internal/`](internal) | The code for the first milestone and its tests. |

## At a glance

| | |
| --- | --- |
| **Role** | Control-plane service |
| **Formerly** | `cs-store`, then Bosun |
| **Namespace** | `clearedstack` |
| **Container image** | `lazarette.clearedstack.svc.cluster.local:5000/shipkeeper` |
| **Memory budget** | 48 MiB |
| **Depends on** | [Plainforge](https://github.com/ClearedStack/plainforge), [Lazarette](https://github.com/ClearedStack/lazarette), [Muster](https://github.com/ClearedStack/muster) |
| **Depended on by** | Nothing else in the platform |

### Boundaries

- Kubernetes API, using server-side apply
- Reads manifests from Plainforge and image digests from Lazarette
- Sends cleaned health reports to Muster

## What works today

One pass, for one app, started by hand:

1. **Read `app.yaml`** and refuse anything that is not understood, with the line number.
2. **Check the digest** against the signed digests Lazarette holds. If it is not there, nothing is sent to the cluster.
3. **Apply** a NetworkPolicy, a Service and a Deployment with server-side apply, as field manager `shipkeeper`. All three are dry-run first, so a refusal leaves nothing half-applied. Shipkeeper never forces an apply: if someone else owns a field with a different value, it reports the conflict and the fields, and leaves their value alone.
4. **Read back** the Deployment, its pods and the namespace's events.
5. **Report health** as `healthy`, `degraded` or `paused`, with node names, pod names and host names, addresses and internal DNS names removed. If anything of that kind is still in the report after cleaning, the report is not sent.

Plainforge, Lazarette and Muster do not exist yet. Each is a small interface in [`internal/platform`](internal/platform/platform.go) with an in-memory stand-in.

**What has and has not been exercised.** Every test runs against [`internal/kubetest`](internal/kubetest/kubetest.go), an in-memory stand-in for the Kubernetes API written for this repository. It is simpler than the real thing (it owns a list as one field, validates nothing, and runs no controllers). Shipkeeper has not yet been run against a real Kubernetes API server. A test for that exists, `TestRealAPIServer`, and is skipped unless you point it at one; it has never been run. No container image has been built with `ko`, and the 48 MiB budget has not been measured in a pod.

### Try it

Needs Go 1.24 or later and nothing else. No network, no cluster.

```sh
go test ./...                      # all tests, including the saved examples
go run ./cmd/shipkeeper-demo       # the milestone against the in-memory cluster; prints the cleaned report
```

Against a cluster you can throw away (the namespace must exist):

```sh
go run ./cmd/shipkeeper \
  -app example-app \
  -manifest internal/example/files/app.yaml \
  -digests internal/example/files/digests.json \
  -kube-url https://your-api-server:6443 \
  -kube-token-file token -kube-ca-file ca.crt
```

The report that would go to Muster is printed on standard output. A refusal is one line on standard error, starting with `E_TIMEOUT`, `E_PARSE` or `E_NEEDS_SUBTOOL`, and the exit status is 1. Run it again to see the health change; applying the same `app.yaml` twice changes nothing.

### The `app.yaml` read today

`SPEC.md` does not say what is in `app.yaml`. This is the format built for the milestone, and it is a proposal:

```yaml
name: example-app            # the app, and the name of its three objects
namespace: cs-workers        # clearedstack or cs-workers, nothing else
image: example-app           # repository name in Lazarette, with no host
digest: sha256:4e7a…         # 64 hex digits; must be a signed digest Lazarette holds
replicas: 2                  # 1 to 10
port: 8080                   # the one port the app listens on
memory: 32Mi                 # request and limit
allowFrom:                   # optional: who may connect; no entry means nobody
  - clearedstack/muster
```

Only a small part of YAML is accepted: comments, `key: value`, a list of plain values, and simple quotes. Anchors, tags, `[…]`, `{…}`, `|`, `>`, `---` and escapes are refused with `E_NEEDS_SUBTOOL`. The reader is in [`internal/yamlsubset`](internal/yamlsubset/yamlsubset.go) and uses the standard library only; the module has no dependencies.

### Saved examples

Every rule has a saved input and the exact output expected. A change in behaviour fails a test. After a deliberate change, `go test ./... -update` rewrites them; review the diff.

| Rules | Saved examples |
| --- | --- |
| Reading `app.yaml`, and the three objects made from it | [`internal/manifest/testdata`](internal/manifest/testdata): 36 cases, `app.yaml` → `objects.json` or `error.txt` |
| Removing cluster details from text | [`internal/sanitize/testdata/cases.json`](internal/sanitize/testdata/cases.json): 39 cases |
| Health state and the report | [`internal/health/testdata`](internal/health/testdata): 11 cases, `observed.json` → `report.json` or `error.txt` |
| The whole pass: requests made and reports sent | [`internal/reconcile/testdata`](internal/reconcile/testdata), from [`internal/example/files`](internal/example/files) |
| Every error Shipkeeper can report | [`internal/errs/testdata/catalogue.txt`](internal/errs/testdata/catalogue.txt) |
| What the command prints | [`cmd/shipkeeper/testdata`](cmd/shipkeeper/testdata) |

### Not yet done

Against `SPEC.md`:

| Requirement | State |
| --- | --- |
| Watch `app.yaml` in Plainforge and digests in Lazarette | Not done. One pass, started by hand. The real components do not exist. |
| Reconcile all 17 workloads | Not done. One app per run. |
| Apply `Deployment`, `Service`, `NetworkPolicy` by server-side apply | Built; tested against the in-memory stand-in only. |
| Install | The first apply does this. Not reachable from Muster. |
| Upgrade, Pause, Rollback | Not done. |
| Watch `Pod`, `Deployment`, `Event` and stream to Muster | Not done. Read once per pass. |
| Health states `healthy`, `degraded`, `paused` | Built. |
| Health state `idle` | Not done: the specification gives no rule for it. |
| Error codes `E_TIMEOUT`, `E_PARSE`, `E_NEEDS_SUBTOOL` | Built for Shipkeeper's own errors. Not derived from pod failures: the specification gives no rule. |
| Failure: pod crash or failed health check | Reported as `degraded` with the cleaned reason. No priority, no Rollback. |
| Failure: server-side apply conflict | Built. |
| Failure: missing or unsigned digest | Built, with the Lazarette stand-in. No signature is checked. |
| Failure: Muster unreachable | Half built: the apply still happens and the failed delivery is reported. No retry. |
| Topology sanitization | Built for the report in this milestone. |
| No public Ingress | Nothing listens at all yet. |
| RBAC, non-root, dropped capabilities, read-only filesystem, 48 MiB, priority class | Not done: there are no deployment manifests for Shipkeeper itself and no `ko` build yet. |

## Names

`shipkeeper` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
