# Backend Reorganization Plan

Target: move from a single flat `cmd/server/` package (~30 files, ~10k lines)
to a layered internal-package structure, without behavior changes, without
breaking the build at any commit.

## Why

Today every file in `cmd/server/` lives in `package main`. Global state
(`clientset`, `cache`, `authEnabled`, `defaultRole`, `restCfg`) flows
implicitly through every handler. New contributors need to scan a flat
directory of large files (workloads.go 1,100+ lines, pods.go 800+, cache.go
450+) to find anything. There are no module boundaries to test against.

## Target layout

```
kube-argus/
├── cmd/
│   └── server/
│       └── main.go              # Wire-up only: parse env, init clients,
│                                 # build mux, ListenAndServe
└── internal/
    ├── k8s/                     # Cluster cache + informers
    │   ├── cache.go             # ← cache.go
    │   ├── informers.go         # ← cache.go (startCacheLoop, etc.)
    │   └── dynamic.go           # ← parts of crds.go
    │
    ├── auth/                    # Sessions, OIDC, role checks
    │   ├── session.go           # ← auth.go
    │   ├── oidc.go              # ← auth.go OIDC flow
    │   ├── google.go            # ← auth.go Google SSO
    │   └── role.go              # requireAdmin, isAdmin, requireAdminOrJIT
    │
    ├── audit/                   # Audit trail
    │   ├── trail.go             # ← audit.go (record, dedup, trim)
    │   ├── persist.go           # ← audit.go ConfigMap I/O
    │   └── presence.go          # ← audit.go WS presence
    │
    ├── jit/                     # Just-in-time access
    │   ├── store.go             # ← jit.go in-memory store
    │   ├── lifecycle.go         # ← jit.go approve/deny/revoke/expire
    │   └── persist.go           # ← jit.go ConfigMap I/O
    │
    ├── notify/                  # Slack + generic webhook
    │   ├── slack.go             # ← slack.go
    │   ├── webhook.go           # ← webhook.go
    │   └── sign.go              # ← webhook.go HMAC signer (already tested)
    │
    ├── api/                     # HTTP handlers
    │   ├── pods.go              # ← pods.go
    │   ├── workloads.go         # ← workloads.go
    │   ├── nodes.go             # ← nodes.go
    │   ├── overview.go          # ← overview.go
    │   ├── networking.go        # ← networking.go
    │   ├── storage.go           # ← storage.go
    │   ├── resources.go         # ← resources.go
    │   ├── yaml.go              # ← yaml_editor.go
    │   ├── ai.go                # ← ai.go
    │   ├── config.go            # ← config.go
    │   ├── crds.go              # ← crds.go
    │   └── exec.go              # ← exec.go
    │
    ├── prom/                    # Prometheus client + caching
    │   └── client.go            # ← prometheus.go
    │
    ├── spot/                    # Spot-instance / cost advisor
    │   └── advisor.go           # ← spot_advisor.go
    │
    └── httpx/                   # HTTP response helpers
        ├── json.go              # j, je, jk8s, jGz
        └── errors.go            # je, jk8s
```

## Cross-package state

Move the globals into `internal/k8s/cluster.go` as struct fields and pass a
`*k8s.Cluster` handle to anything that needs them:

```go
// internal/k8s/cluster.go
package k8s

type Cluster struct {
  Clientset       kubernetes.Interface
  Dynamic         dynamic.Interface
  Apiext          apiextclient.Interface
  Metrics         metricsv.Interface
  RestConfig      *rest.Config
  Cache           *ClusterCache
}
```

Each `internal/api/*` constructor takes whatever it needs:

```go
// internal/api/pods.go
func NewHandler(cluster *k8s.Cluster, audit *audit.Trail) *Handler { ... }
```

The wire-up moves to `cmd/server/main.go`:

```go
func main() {
  cluster := k8s.NewCluster(...)
  audit := audit.NewTrail(cluster)
  jit := jit.NewStore(cluster)
  auth := auth.NewService(...)
  // ...
  mux := http.NewServeMux()
  podsHandler := api.NewPods(cluster, audit, auth)
  mux.HandleFunc("/api/pods", podsHandler.List)
  mux.HandleFunc("/api/pods/", podsHandler.Detail)
  // ...
}
```

## Execution order (4 commits, each green)

### Commit 1 — Seam: extract `internal/httpx/`

Smallest possible move that proves the pattern. Move `j`, `je`, `jk8s`, `jGz`
out of `helpers.go` into `internal/httpx/json.go`. Find/replace `j(w, x)` →
`httpx.JSON(w, x)` across all callers. ~30 files touched, all mechanical.

Done when: `go test ./...` and `go vet ./...` pass.

### Commit 2 — Extract `internal/notify/`

Slack + webhook are both leaf packages — they call out to network and the
audit log, but nothing reads from them. Move:
- `slack.go` → `internal/notify/slack.go`
- `webhook.go` → `internal/notify/webhook.go`
- `webhook_test.go` → `internal/notify/webhook_test.go`

Export the public surface (`NotifyJIT`, `Init`, `Settings*`). Update callers
in `jit.go` and `main.go`.

Done when: tests pass, webhook still fires end-to-end.

### Commit 3 — Extract `internal/auth/` and `internal/audit/`

Most-touched globals (`authEnabled`, `defaultRole`, `requireAdmin`,
`isAdmin`, `auditRecord`). Migrate together to avoid an intermediate state
where some files import `internal/auth` and others still use the package
main globals.

Done when: tests pass, login flow works, audit entries land in the ConfigMap.

### Commit 4 — Extract `internal/k8s/` + `internal/api/*`

The big one. Cache, informers, all HTTP handlers. Each handler subpackage
gets its own `*Handler` struct so handler methods can hold state explicitly.

Done when: full e2e flow works against a real cluster.

## Test hygiene during the move

After each commit:
1. `go build ./...` — clean
2. `go vet ./...` — clean
3. `go test ./...` — all 19 existing tests still pass
4. Manual smoke: build the image, deploy to a kind cluster, hit `/api/overview`

If any step fails, revert that commit. Don't ship a broken intermediate state.

## What NOT to do

- **Don't change interfaces while moving.** Keep function signatures stable
  in commit N; refactor signatures in a separate commit N+1. This keeps each
  commit reviewable as "move only" or "logic only."
- **Don't introduce DI containers or interfaces "for testability"** until
  there's a concrete test that demands it. Adding interfaces speculatively
  bloats the codebase.
- **Don't merge two layers at once** (e.g., `internal/jit/` and
  `internal/notify/` together). Each merge is a rebase hazard if the user is
  shipping in parallel.

## Time estimate

- Commit 1 (httpx seam): 1-2 hours
- Commit 2 (notify): 2 hours
- Commit 3 (auth + audit): 3-4 hours
- Commit 4 (k8s + api): 6-8 hours

**Total: ~1.5-2 days of focused work.**

A subagent prompt that can execute this:

> Execute commit 1 of the kube-argus backend reorganization plan in
> `docs/REORGANIZATION.md`. Move `j`, `je`, `jk8s`, `jGz` from
> `cmd/server/helpers.go` to a new `internal/httpx/` package. Update all
> callers. `go build ./...`, `go vet ./...`, and `go test ./...` must remain
> clean. Do not change behavior. Return when done with a list of files
> touched and the new package's public surface.
