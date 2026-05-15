# AGENTS.md

This file provides guidance to coding agents (e.g. Claude Code, claude.ai/code) when working with code in this repository.

## Repository purpose

Go module `kubeops.dev/sidekick` — defines a `Sidekick` CRD (`apps.k8s.appscode.com/v1alpha1.Sidekick`) and reconciles it into a sibling Pod that runs alongside a "leader" Pod selected by labels. Used to attach helper workloads (a config reloader, a one-shot init, a periodic job) to existing Pods without modifying their owning controller. The Sidekick spec includes both **standard** mode (one Pod per leader) and **distributed** mode for fan-out cases.

The README is a Kubebuilder scaffold stub; treat this file as the source of truth.

The produced binary is `sidekick`.

## Architecture

- `cmd/sidekick/` — entry point.
- `pkg/cmds/` — Cobra root + run.
- `apis/apps/v1alpha1/`:
  - `sidekick_types.go` — `Sidekick`.
  - `register.go`, `crds.go`, `doc.go`, `groupversion_info.go`, generated `zz_generated.deepcopy.go` / `openapi_generated.go`.
  - `install/`, `fuzzer/` — scheme registration + fuzz helpers.
- `client/` — generated typed clientset.
- `crds/` — generated CRD YAML (`apps.k8s.appscode.com_sidekicks.yaml`) + `lib.go`.
- `pkg/controllers/apps/`:
  - `sidekick_controller.go` — main reconciler.
  - `sidekick.go` — Sidekick resolution helpers.
  - `pod.go` — Pod creation/update logic for the sidekick.
  - `distributed.go` — distributed mode (one sidekick per matching leader pod, balanced across nodes).
  - `suite_test.go` — envtest harness.
- `Dockerfile.in` (PROD, distroless), `Dockerfile.dbg` (debian), `Dockerfile.ubi` (Red Hat certified) — three image variants.
- `PROJECT` — Kubebuilder metadata.
- `hack/`, `Makefile` — AppsCode build harness.
- `vendor/` — checked-in deps.

CRD API group is `apps.k8s.appscode.com` (matches the project's domain convention; do **not** collide with vanilla `apps`).

## Common commands

All Make targets run inside `ghcr.io/appscode/golang-dev` — Docker must be running.

- `make ci` — CI pipeline.
- `make build` / `make all-build` — build host or all-platform binaries.
- `make gen` — regenerate clientset + manifests. Run after any change to `apis/apps/v1alpha1/*_types.go`.
- `make manifests` — regenerate CRDs only.
- `make clientset` — regenerate `client/` only.
- `make fmt`, `make lint`, `make unit-tests` / `make test` — standard.
- `make verify` — `verify-gen verify-modules`; `go mod tidy && go mod vendor` must leave the tree clean.
- `make container` — build PROD, DBG, and UBI images.
- `make push` — push all three; `make docker-manifest` writes multi-arch manifests; `make release` is the full publish flow.
- `make push-to-kind` / `make deploy-to-kind` — load into Kind and Helm-install.
- `make install` / `make uninstall` / `make purge` — Helm install lifecycle.
- `make add-license` / `make check-license` — manage license headers.

Run a single Go test (requires a local Go toolchain):

```
go test ./pkg/controllers/apps/... -run TestName -v
```

## Conventions

- Module path is `kubeops.dev/sidekick` (vanity URL). Imports must use that.
- License: Apache-2.0 (`LICENSE`); new files need the standard "Copyright AppsCode Inc. and Contributors" header (`make add-license`).
- Sign off commits (`git commit -s`); contributions follow the DCO (`DCO`).
- Vendor directory is checked in — `go mod tidy && go mod vendor` must leave the tree clean (enforced by `verify-modules`).
- CRD API group is `apps.k8s.appscode.com`. Use the AppsCode domain — do not collide with the upstream `apps` group.
- Distributed-mode logic lives in `pkg/controllers/apps/distributed.go`. Don't merge it back into the standard reconciler; the two flows have different invariants.
- Do not hand-edit `zz_generated.*.go`, `openapi_generated.go`, anything under `client/`, or `crds/` — change `apis/apps/v1alpha1/*_types.go` and re-run `make gen`.
- Three Dockerfiles, one binary — keep `Dockerfile.in`, `Dockerfile.dbg`, and `Dockerfile.ubi` in sync.
- This is a **Kubebuilder project** — use `kubebuilder` to scaffold new APIs.
