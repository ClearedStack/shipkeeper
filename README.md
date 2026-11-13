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

> **Status: design.** Nothing is built yet. This repository holds the specification and the prompt for building it, and no code.

Shipkeeper is a control-plane service of the ClearedStack platform. ClearedStack is an open pipeline that finds ideas for government-contracting tools, screens and scores them, and builds each one from the public rule. Shipkeeper is meant to be understood and used without the rest.

## In this repository

| File | What it is |
| --- | --- |
| [`SPEC.md`](SPEC.md) | The specification: what Shipkeeper does, its interfaces, how it fails and how it is kept safe. |
| [`PROMPT.md`](PROMPT.md) | The prompt to start a build session with. It says what to build and the rules to build it under. |
| [`component.json`](component.json) | This component’s entry in the ClearedStack naming registry. |
| [`brand/`](brand) | The Shipkeeper logo and mark, for light and dark backgrounds. |

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

## Names

`shipkeeper` is this component’s identifier everywhere: the repository, the workload, the container image. The one place names are written down is the [naming registry](https://clearedstack.com/platform/registry.json), kept in [`clearedstack.com`](https://github.com/ClearedStack/clearedstack.com/blob/main/src/data/registry.json). `component.json` here is a copy of this component’s entry. If the two disagree, the registry is right.

## Ground rules

- **No AI when it runs.** The same input gives the same output on any machine.
- **Stop instead of guessing.** A case with no rule is paused and reported, never improvised.
- **Clean room.** Written from scratch. No code, text or design is copied from another project.
- **Public sources and made-up data only.** Nothing classified, controlled or taken from a real contract, in code, tests, issues or commit messages.

## License

[MIT](LICENSE). ClearedStack is an independent personal project. It is not affiliated with, endorsed by, or sponsored by the U.S. Department of Defense or any other government agency.
