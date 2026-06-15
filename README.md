# poll-ci

**A dead-simple, single-container CI for GitHub.** It watches your repos by
**polling** — outbound only, no webhooks, no inbound ports — runs each repo's
checks in Docker, and reports results as native **GitHub commit statuses**.

One process. One container. No web UI, no database, no webhook listener. The
whole thing is a ~6 MB Go binary with a single dependency, readable end to end
in one sitting.

```
 push ─▶ GitHub ◀── git ls-remote (every POLL_INTERVAL) ── poll-ci
                ◀── POST commit status ──────────────────────┘  │
                                                                 ▼
                                            docker run <your image>: ci/test ✓  ci/lint ✗
```

![release](https://img.shields.io/github/v/release/extremeshok/poll-ci)
![license](https://img.shields.io/badge/license-MIT-blue)
![go](https://img.shields.io/badge/go-1.26-00ADD8)
![image](https://img.shields.io/badge/ghcr.io-poll--ci-blue?logo=docker&logoColor=white)
![ports](https://img.shields.io/badge/inbound%20ports-none-success)

---

## Contents

- [Why poll-ci](#why-poll-ci)
- [How it works](#how-it-works)
- [Quickstart (60 seconds)](#quickstart-60-seconds)
- [Tutorial: your first green check](#tutorial-your-first-green-check)
- [The `.poll-ci.yml` file](#the-poll-ciyml-file)
- [Example configs](#example-configs)
- [Configuration](#configuration)
- [Deploying](#deploying)
- [Token setup](#token-setup)
- [Required status checks](#required-status-checks)
- [Operating poll-ci](#operating-poll-ci)
- [Security](#security)
- [Troubleshooting](#troubleshooting)
- [Limitations](#limitations)
- [FAQ](#faq)
- [Building from source](#building-from-source)
- [License](#license)

---

## Why poll-ci

For when **GitHub Actions is blocked, metered, or out of credits**, when you're
**behind NAT or a firewall** with no inbound ports to spare, or when you just
want green check-marks without standing up infrastructure.

Self-hosted CIs (Woodpecker, Drone, Forgejo Actions, Jenkins) are powerful and
much bigger: a server, agents, a web UI, a database, and inbound webhooks.
poll-ci is the deliberate **minimal opposite** — it dials *out* to GitHub on a
timer and reports back.

|                         | poll-ci            | GitHub Actions      | Woodpecker / Drone / Jenkins |
|-------------------------|--------------------|---------------------|------------------------------|
| Inbound ports / webhooks | **None** (polls)  | n/a (hosted)        | Webhooks (inbound)           |
| Works behind NAT        | **Yes**            | Yes                 | Needs reachable endpoint     |
| Infra to run            | **1 container**    | None (it's hosted)  | Server + agents + DB + UI    |
| Web UI / dashboard      | No                 | Yes                 | Yes                          |
| Config                  | Flat list of checks| YAML workflows      | Pipeline DSL                 |
| Cost                    | Your compute       | Metered minutes     | Your compute                 |

**Use poll-ci when** you have a handful of repos, want results posted to GitHub,
and value "one small thing I fully understand" over features. **Reach for the
bigger tools when** you need a UI, fan-out matrices, artifacts, or a shared
team CI platform.

### What it does *not* do (on purpose)

No web UI · no webhooks or inbound ports · no database or queue · no
server↔agent split · no pipeline DSL (stages, `depends-on`, matrices, plugins,
conditionals, secret vaults) · no artifact/log hosting · no build caching.
Just a flat list of named checks. Keeping these out is the whole point.

---

## How it works

Every `POLL_INTERVAL` seconds, for each watched branch:

1. **Read HEAD** — `git ls-remote` returns the branch's tip SHA (a cheap
   outbound call). If poll-ci has already tested that SHA, it does nothing.
2. **Check out** — on a new SHA, it shallow-clones that commit.
3. **Read config** — `.poll-ci.yml` (or `.poll-ci.yaml`) from the repo root.
4. **Announce** — posts a `pending` status for the `ci` rollup and each
   `ci/<check>`.
5. **Run** — for each check, it creates a container from the repo's `image:`,
   streams the checkout in over `docker cp`, runs the command, and captures the
   exit code and output.
6. **Report** — posts `success`/`failure` per check with a one-line summary,
   then an overall `ci` rollup (`success` only if every check passed). Posts
   are retried on transient failures, so a network blip can't strand a
   context on `pending`.
7. **Remember** — records the SHA so it never runs twice, and survives restarts
   (entries older than ~6 months are pruned; a branch's last result never is).

Sources reach the check container over the Docker API (a streamed tar), so the
canonical deployment needs **nothing mounted but the Docker socket** — no shared
host work directory.

### What it looks like on GitHub

On the commit (and on any PR that includes it), you get native status checks:

```
✓ ci          all 3 checks passed
✓ ci/build    passed in 12s
✓ ci/test     passed in 48s
✗ ci/lint     exit 1: main.go:42:6: undefined: foo
```

These are real commit statuses — usable as **required checks** in branch
protection, shown in the PR merge box, and visible via the API.

---

## Quickstart (60 seconds)

You need Docker, a GitHub repo, and a token (see [Token setup](#token-setup)).

```bash
# 1. Get the engine image — pull the prebuilt multi-arch image:
docker pull ghcr.io/extremeshok/poll-ci:latest
#    (or build it yourself: git clone the repo and `docker build -t poll-ci .`)

# 2. Add a .poll-ci.yml to the repo you want to test (commit & push it):
cat > .poll-ci.yml <<'YML'
image: golang:1.26
checks:
  - name: test
    run: go test ./...
YML

# 3. Run the watcher
docker run -d --name poll-ci --restart always \
  -e GITHUB_TOKEN=github_pat_xxx \
  -e REPO=you/your-repo \
  -e BRANCH=main \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v poll-ci-state:/var/lib/poll-ci \
  ghcr.io/extremeshok/poll-ci:latest
```

Push to that branch and, within `POLL_INTERVAL` (default 60s), the `ci/*`
statuses appear on the commit — `pending` first, then `success`/`failure` —
plus the overall `ci` rollup.

Follow along with `docker logs -f poll-ci`. (Later examples write `poll-ci` as
the image name for brevity — that's the same image: the prebuilt
`ghcr.io/extremeshok/poll-ci:latest`, or your local `docker build -t poll-ci .`)

> **The two volumes.** The Docker socket lets poll-ci start your check
> containers (see [Security](#security)). The `poll-ci-state` volume persists
> the set of tested commits so a restart doesn't re-run them. Drop it and the
> bare socket-only form still works — it just re-tests the current HEAD once
> after each restart.

> **Deploying for real?** Skip the manual `docker run` and use the **one-command
> installer** for a boot-managed systemd service:
>
> ```bash
> git clone https://github.com/extremeshok/poll-ci && sudo poll-ci/deploy/install.sh
> ```
>
> It installs the systemd unit, pins the published image, and creates a 0600 env
> file for your token — see [As a Docker service (systemd)](#as-a-docker-service-systemd).

---

## Tutorial: your first green check

A complete walk-through, from nothing to a green check-mark.

**1. Create a token.** A fine-grained PAT scoped to one repo, with
**Contents: Read-only** and **Commit statuses: Read and write**
(full steps in [Token setup](#token-setup)). Copy it.

**2. Add checks to your repo.** In the repo you want to test, create
`.poll-ci.yml` at the root. Here's one that always passes and one that always
fails, so you can see both colors:

```yaml
image: alpine:3.24
checks:
  - name: hello
    run: echo "poll-ci is working"
  - name: willfail
    run: exit 1
```

Commit and push it.

**3. Start the watcher**, pointing it at your repo (run from a host with
Docker; a throwaway VM is ideal — see [Security](#security)):

```bash
docker run -d --name poll-ci --restart always \
  -e GITHUB_TOKEN=github_pat_xxx \
  -e REPO=you/your-repo -e BRANCH=main \
  -e POLL_INTERVAL=15 \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v poll-ci-state:/var/lib/poll-ci \
  poll-ci
```

**4. Watch it work:**

```bash
docker logs -f poll-ci
# [you/your-repo@main] new commit a1b2c3d
# [you/your-repo@main] a1b2c3d ci/hello → success (passed in 0s)
# [you/your-repo@main] a1b2c3d ci/willfail → failure (exit 1: ...)
```

**5. See it on GitHub.** Open the commit (or run
`gh api repos/you/your-repo/commits/main/status --jq '.state'`). You'll see
`ci/hello` green, `ci/willfail` red, and the `ci` rollup red.

**6. Make it green.** Remove the `willfail` check, push again, and within
`POLL_INTERVAL` the commit goes all-green. That's it — you have CI.

---

## The `.poll-ci.yml` file

Lives at the **root of each watched repo** (named `.poll-ci.yml` or
`.poll-ci.yaml`). This is the entire schema:

```yaml
image: golang:1.26        # toolchain container the checks run in
                          #   optional → defaults to DEFAULT_IMAGE (alpine:latest)
checks:
  - name: test            # → commit status context "ci/test"
    run: go test ./...    # command, run as `sh -ec` inside `image`
    timeout: 900          # seconds (optional → DEFAULT_TIMEOUT, 1800)
  - name: lint
    run: golangci-lint run
scan: {}                  # optional: trivy source scan → "ci/trivy" (see below)
promote:                  # optional: fast-forward a branch on green (see below)
  branch: release
```

**Rules and behavior:**

- Each check becomes a commit status `ci/<name>`. An overall `ci` status rolls
  them up (`success` only if **every** check passed).
- A check **passes if its command exits `0`**. On failure the status
  description is the last line of output (stderr preferred) — e.g.
  `exit 1: FAIL: expected 4 but got 5`. A check that exceeds its `timeout` is a
  failure (`timed out after 15m0s`) and its container is killed.
- Checks run **in parallel by default** (up to `CHECK_CONCURRENCY`, default: the
  host CPU count), **each in its own fresh container** built from `image`, with the commit's files
  at `/repo` (the working directory). Set `CHECK_CONCURRENCY=1` to run them one at
  a time. `.git` is included too, but it's a **shallow, depth-1 clone** — full
  history and tags aren't fetched, so `git describe --tags` and `git log` beyond
  the tip won't work.
- **Concurrency and per-check resources.** By default a commit's checks run up to
  `CHECK_CONCURRENCY` (default: the host CPU count) at a time and **share the host
  CPUs** — checks are *not* pinned to one core, so a `go test` or a bundler still
  uses several.
  A check may *optionally* set `cpus:` and `memory:` (docker `--cpus`/`--memory`)
  to cap itself; `cpus:` is then also its **scheduler weight**, and poll-ci admits
  checks while their summed `cpus` stays within `CHECK_CPU_BUDGET` (default: the
  host CPU count) — so a heavy check (`cpus: 4`) reserves its slice and light ones
  pack densely. (Quote the values, e.g. `cpus: "2"`.) Under parallelism each
  check's output is buffered and printed as one labeled block; a single serial
  check (`CHECK_CONCURRENCY=1`) streams live as before.
- **There is no shared workspace between checks** — this is not a pipeline. A
  check that needs dependencies must install them itself. Combine setup with the
  check rather than splitting them:

  ```yaml
  # ✗ "test" runs in a clean container and won't see install's node_modules
  - { name: install, run: npm ci }
  - { name: test,    run: npm test }

  # ✓ self-contained
  - { name: test, run: npm ci && npm test }
  ```

  (Tools like `go build` / `go test` are naturally self-contained.)
- **One `image:` per repo** — all of a repo's checks share it. For a polyglot
  repo, use an image that has every toolchain you need, or `install` what each
  check needs inside its `run`.
- `run` may be multi-line YAML (`run: |`). It is **not** interpolated by
  poll-ci; it's handed verbatim to `sh -ec` in the container, so normal shell
  (`&&`, `|`, `$(...)`, env vars) works.

### Scanning the source with trivy (`scan:`)

Add a `scan:` block and poll-ci runs a [trivy](https://trivy.dev) **filesystem
scan of the checkout** as a built-in gate step, reported as the **`ci/trivy`**
commit status. A finding fails the gate exactly like a failing check — so paired
with `promote:`, vulnerable dependencies, leaked secrets, or misconfigured
Dockerfiles/IaC never advance a deploy branch. This is shift-left scanning of the
**source** (manifests, secrets, config), complementary to scanning built images
at deploy time. No GitHub token is needed.

Minimal — every field defaults:

```yaml
scan: {}
```

Full form, defaults shown:

```yaml
scan:
  image: aquasec/trivy:latest       # the trivy image (pin a digest for reproducibility)
  scanners: vuln,secret,misconfig   # trivy --scanners
  severity: HIGH,CRITICAL           # trivy --severity (a finding at/above this fails)
  ignore-unfixed: true              # only fail on vulnerabilities that have a fix
  path: .                           # sub-path under the repo to scan
  timeout: 1800                     # seconds (optional → DEFAULT_TIMEOUT)
  cache-volume: poll-ci-trivy-cache # named Docker volume for the trivy DB cache
```

- Runs after the checks, in the trivy image, against the copied-in `/repo` —
  using trivy's own entrypoint with **argv passing (no shell)**, so the config
  values can't smuggle shell metacharacters (they're charset-validated too).
- The vulnerability DB lives in the `cache-volume` named volume, so it isn't
  re-downloaded every run; trivy still refreshes it from its registry as needed,
  so coverage stays current even when the trivy `image` is pinned.
- Like checks, the scan **needs network** (first run downloads the DB) and runs
  beside the Docker socket — so, as with checks, only point poll-ci at repos you
  trust.

### Caching dependencies (`cache:`)

To avoid re-downloading dependencies every run, poll-ci can keep a **clean
"golden" cache** per repo+branch and **copy it into each check container**. It is
**opt-in** — enable it with `CACHE=true` (and tune per repo with `cache:`).

> **When caching helps — and when it doesn't.** Because poll-ci is mount-free, the
> golden cache is **copied into every check container** (the price of needing
> nothing mounted but the Docker socket). That copy is only worth it when your
> checks are **download-dominated**. For **compute-bound** checks — `go test`,
> coverage, a webpack/vite build — the wall-clock is the compilation, which
> caching can't shorten, and at high `CHECK_CONCURRENCY` the concurrent copy-in
> can even add I/O contention. Measure your repo before enabling it; **parallelism
> (on by default) is the bigger, universal win.** Leave `CACHE` off unless a check
> spends real time on `npm ci` / `go mod download` / `pip install` over the
> network.

How it stays clean (and unpoisonable):

- The golden cache is built by a **prime command** run in an *isolated* container,
  then copied into each check as a **private copy**. **Checks never write the
  golden back**, so a run — or a PR — can't poison it.
- It's **auto-detected** from ecosystem marker files: `go.mod` → `/go/pkg/mod` +
  `/root/.cache/go-build`, primed with `go mod download`; npm / yarn / pnpm / pip
  / poetry / cargo are detected similarly. Override per repo:

  ```yaml
  cache:
    paths: [/go/pkg/mod, /root/.cache/go-build]   # container dirs to persist
    prime: go mod download                        # run in a clean container to populate them
  ```

- The golden is **re-primed only on watched-branch runs**, and only when the
  lockfiles change (their content hash differs) or `CACHE_REFRESH_HOURS`
  (default 24) lapses. **PR and `--once` runs read it but never prime/write it.**
- It's a **speed optimization only** — a check must still pass from a cold cache.
  The golden lives under `WORK_DIR/cache/`; delete it to reset (it grows over
  time). Priming is best-effort: if the prime command fails, the run just
  proceeds cold.

> **Caveat — caching + parallel checks:** content-addressed caches (the Go
> build/module cache) tolerate concurrent checks; others (e.g. `node_modules`)
> may not. Prefer caching the *download* cache (`~/.npm`, `~/.cache/pip`), or keep
> such checks serial.

### Promoting a deploy branch on green (`promote:`)

Optionally, when **every check passes on a watched branch**, poll-ci can
fast-forward another branch to the tested commit. This turns "green on `master`"
into "`release` updated", so a deploy poller on your server can ship it — a
fully poll-based, GitHub-Actions-free CD pipeline:

```yaml
image: golang:1.26
checks:
  - name: test
    run: go test ./...
promote:
  branch: release         # on green of the watched branch, fast-forward this branch
```

- Runs **only for branch commits** — never for PR heads (`POLL_PRS`) or `--once`.
- The update is **fast-forward only** (GitHub rejects a non-fast-forward), so a
  `release` someone moved by hand is never clobbered — poll-ci reports the
  refusal instead. If the target branch doesn't exist yet, it's created.
- The outcome is its own commit status, **`ci/promote`** (`success` / `error`),
  alongside the per-check statuses.
- This needs the token to have **`Contents: write`** (status reporting alone only
  needs `Commit statuses: write` + `Contents: read`). Promotion is opt-in, so the
  extra scope is required only when a repo asks for it; a token without it yields
  a clear `ci/promote` failure rather than a silent no-op.

#### Heartbeat (dead-man's-switch for the promotion pipeline)

poll-ci is a single container on one host holding one token. If it dies, the
token expires, or the gate silently goes red, `release` just stops advancing and
nobody is paged. [`deploy/heartbeat-monitor.sh`](deploy/heartbeat-monitor.sh)
(run from [`poll-ci-heartbeat.timer`](deploy/systemd/poll-ci-heartbeat.timer))
closes that gap. It alerts when **either** the poll-ci container is not running,
**or** the watched branch has led the target branch by more than `MAX_LAG`
commits **and** the target has not advanced for longer than `GRACE` (so a gate
that is merely in progress doesn't page you — only a genuine stall does). On
every healthy run it can also ping an external dead-man's-switch
(`HEARTBEAT_PING_URL`, e.g. healthchecks.io), so you're covered even if the
monitor or the whole host dies.

```bash
sudo deploy/install.sh --heartbeat     # installs the script + timer + heartbeat.env
sudo "$EDITOR" /etc/poll-ci/heartbeat.env   # set HEARTBEAT_ALERT_URL (and repo)
```

It reuses `GITHUB_TOKEN` from `poll-ci.env`; config (`HEARTBEAT_REPO`,
`HEARTBEAT_ALERT_URL`, `HEARTBEAT_PING_URL`, thresholds) lives in
`/etc/poll-ci/heartbeat.env`. Exits 0 always — a monitor must never crash its own
timer.

---

## Example configs

Copy one into your repo as `.poll-ci.yml`. Ready-made files live in
[`examples/`](examples/).

**Go** — [`examples/go`](examples/go/.poll-ci.yml)

```yaml
image: golang:1.26
checks:
  - { name: build, run: go build ./... }
  - { name: vet,   run: go vet ./... }
  - { name: test,  run: go test ./..., timeout: 600 }
```

**Node** — [`examples/node`](examples/node/.poll-ci.yml)

```yaml
image: node:22
checks:
  - { name: test, run: npm ci && npm test, timeout: 600 }
  - { name: lint, run: npm ci && npm run lint --if-present }
```

**Python** — [`examples/python`](examples/python/.poll-ci.yml)

```yaml
image: python:3.12
checks:
  - { name: test, run: pip install --no-cache-dir -r requirements.txt && pytest -q, timeout: 600 }
  - { name: lint, run: pip install --no-cache-dir ruff && ruff check . }
```

**Rust**

```yaml
image: rust:1
checks:
  - { name: test,  run: cargo test --all, timeout: 900 }
  - { name: clippy, run: rustup component add clippy && cargo clippy -- -D warnings }
```

**A plain shell / Makefile project**

```yaml
image: alpine:3.24
checks:
  - { name: ci, run: apk add --no-cache make && make test }
```

---

## Configuration

All configuration is environment variables.

| Variable          | Default                  | Meaning                                                       |
|-------------------|--------------------------|---------------------------------------------------------------|
| `GITHUB_TOKEN`    | *(required)*             | Token with Contents:read + Commit statuses:write              |
| `REPO`            | —                        | Single repo to watch, `owner/name`                            |
| `BRANCH`          | `main`                   | Branch for `REPO`                                             |
| `REPOS_FILE`      | —                        | Path to a `repos.yml` (instead of `REPO`/`BRANCH`)            |
| `POLL_INTERVAL`   | `60`                     | Seconds between polls                                         |
| `DEFAULT_IMAGE`   | `alpine:latest`          | Image used when a `.poll-ci.yml` omits `image:`               |
| `DEFAULT_TIMEOUT` | `1800`                   | Per-check timeout (seconds) when `timeout:` is omitted        |
| `WORK_DIR`        | `/var/lib/poll-ci`       | Scratch clones + state file (mount a volume here to persist)  |
| `STATE_FILE`      | `$WORK_DIR/state.json`   | Where the tested-SHA set is stored                            |
| `POLL_PRS`        | `false`                  | Also test open **same-repo** PR heads (see [Security](#security)) |
| `SHORT_CIRCUIT`   | `true`                   | Abort a run when a newer commit lands mid-run and jump to it ([why](#short-circuiting-superseded-runs)) |
| `MARK_SKIPPED`    | `false`                  | Post a terminal `ci` status on never-tested intermediate commits ([details](#marking-skipped-commits-mark_skipped)) |
| `CONCURRENCY`     | `1`                      | Refs swept in parallel — useful with a multi-repo `REPOS_FILE` |
| `CHECK_CONCURRENCY` | *(host CPU count)*     | A commit's checks run in parallel up to this many; `1` = serial (old behavior) |
| `CHECK_CPU_BUDGET` | *(host CPU count)*      | Max summed per-check `cpus` admitted at once across a commit ([details](#the-poll-ciyml-file)) |
| `CACHE`           | `false`                  | **Opt-in** clean dependency-cache copy-in; `CACHE=true` enables it ([when it helps](#caching-dependencies-cache)) |
| `CACHE_REFRESH_HOURS` | `24`                 | Re-prime the golden cache at least this often (`0` = only when missing/stale) |
| `CHECK_MEMORY`    | *(unlimited)*            | Global `docker --memory` for check/scan containers, e.g. `2g` (per-check `memory:` overrides) |
| `CHECK_CPUS`      | *(serial: unlimited)*    | Global `docker --cpus` in **serial** mode; in parallel each check is capped to its `cpus` cost |
| `CHECK_PIDS`      | *(unlimited)*            | `docker --pids-limit` for check/scan containers, e.g. `4096`  |
| `IMAGE_REFRESH_HOURS` | `0` (never)          | Re-pull present check images this often, so `:latest` tags don't freeze at first pull |
| `GITHUB_API`      | `https://api.github.com` | API base — set for GitHub Enterprise Server                   |
| `GIT_HOST`        | `github.com`             | Git host for clones — set for GHES                            |
| `DOCKER_BIN`      | `docker`                 | Docker CLI to invoke                                          |

### Short-circuiting superseded runs

On a busy branch, commits can land faster than CI finishes. By default poll-ci
**short-circuits**: while a run is in flight it keeps watching the branch tip,
and the moment a newer commit appears it **aborts the stale run** (killing the
in-flight check container), marks every unfinished status on that commit
`superseded by <sha>` (checks that had already finished keep their real result),
and jumps straight to the newest commit — so you never wait on CI for a commit
that has already been replaced.

The tip is re-checked every `POLL_INTERVAL` seconds during a run, so only runs
longer than one interval get cut short. Set `SHORT_CIRCUIT=false` to disable it
and run every commit you start to completion. With `POLL_PRS`, PR runs get the
same treatment: a force-push to the PR head aborts the now-stale run.

A superseded commit is *not* remembered as processed: if the branch is later
force-pushed **back** to it (a revert), it re-runs for real instead of keeping
its stale "superseded" statuses forever.

### Marking skipped commits (`MARK_SKIPPED`)

poll-ci tests branch **HEADs** only, so the intermediate commits of a fast
multi-commit push get no statuses at all — ambiguous on the commits page, and
they ride into a `promote:` branch untested. With `MARK_SKIPPED=true`, after
each branch run poll-ci compares the previously tested commit to the new one
and posts a single terminal `ci` status on every commit in between:

```
✗ ci    not tested — superseded by a1b2c3d
```

It is an `error` status on purpose — a green check would let branch protection
pass a commit that never ran. Off by default.

### Watching multiple repos

Point `REPOS_FILE` at a small list instead of `REPO`/`BRANCH`:

```yaml
# repos.yml
repos:
  - { repo: your-org/api, branch: main }
  - { repo: your-org/web, branch: develop }
  - { repo: you/side-project, branch: main }
```

```bash
docker run -d --name poll-ci --restart always \
  -e GITHUB_TOKEN=github_pat_xxx \
  -e REPOS_FILE=/config/repos.yml \
  -v $PWD/repos.yml:/config/repos.yml:ro \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v poll-ci-state:/var/lib/poll-ci \
  poll-ci
```

### Run a single commit and exit (`--once`)

For ad-hoc runs, debugging a `.poll-ci.yml`, or re-running a commit. It ignores
state and exits non-zero if any check failed:

```bash
docker run --rm \
  -e GITHUB_TOKEN=github_pat_xxx -e REPO=you/your-repo \
  -v /var/run/docker.sock:/var/run/docker.sock \
  poll-ci --once <commit-sha>
```

`poll-ci --version` prints the build version.

---

## Deploying

### Container images

Prebuilt, multi-arch (`linux/amd64` + `linux/arm64`) images are published to the
GitHub Container Registry:

```
ghcr.io/extremeshok/poll-ci:latest    # newest release
ghcr.io/extremeshok/poll-ci:v1.5.0    # pin to a specific version (recommended for prod)
```

Prefer building your own? `docker build -t poll-ci .` from a checkout — the
[`Dockerfile`](Dockerfile) is a small multi-stage build (binary + git + Docker CLI).

### docker run

The [Quickstart](#quickstart-60-seconds) command, with `--restart always`, is a
complete deployment. It survives reboots (Docker restarts the container) and
restarts (the state volume means tested commits aren't re-run).

### docker compose

Use the included [`docker-compose.yml`](docker-compose.yml):

```bash
export GITHUB_TOKEN=github_pat_xxx
cp repos.example.yml repos.yml      # then edit it
docker compose up -d               # pulls ghcr.io/extremeshok/poll-ci
```

### As a Docker service (systemd)

To have **systemd** start/stop the container — boot integration, `systemctl`
control, logs via `journalctl` — instead of relying on Docker's `--restart`, use
the bundled unit + installer in [`deploy/`](deploy):

```bash
# from a checkout on the host:
sudo deploy/install.sh             # install + enable  (--build to build the
                                   # image first, --start to start right away)
sudoedit /etc/poll-ci/poll-ci.env  # set GITHUB_TOKEN  (this file is 0600)
sudoedit /etc/poll-ci/repos.yml    # the repo(s) to watch
sudo systemctl start poll-ci
journalctl -u poll-ci -f
```

`install.sh` writes config under `/etc/poll-ci/` (never overwriting an existing
env file), installs [`poll-ci.service`](deploy/systemd/poll-ci.service), and pins
the image with a drop-in. The unit runs the container in the **foreground**, so
`systemctl stop` is a graceful `docker stop` — poll-ci catches SIGTERM, the
in-flight check unwinds, and it simply re-runs that commit on the next start.
Manage it the usual way:

```bash
sudo systemctl start|stop|restart|status poll-ci
```

### As a plain binary (systemd)

poll-ci is a single binary; it shells out to `git` and `docker`, both of which
must be on `PATH` (and the user must be able to run `docker`). Install it
directly if you'd rather not containerize the engine — grab a prebuilt binary
from the [releases page](https://github.com/extremeshok/poll-ci/releases), or
`go install`:

```bash
# Prebuilt (Linux x86-64; see releases for other OS/arch + newer versions).
# The tarball also contains README.md + LICENSE.
curl -fsSL https://github.com/extremeshok/poll-ci/releases/download/v1.5.0/poll-ci_v1.5.0_linux_amd64.tar.gz | tar -xz
sudo install poll-ci /usr/local/bin/

# …or from source:
go install github.com/extremeshok/poll-ci@latest   # to $(go env GOPATH)/bin
```

```ini
# /etc/systemd/system/poll-ci.service
[Unit]
Description=poll-ci
After=docker.service
Wants=docker.service

[Service]
Environment=GITHUB_TOKEN=github_pat_xxx
Environment=REPO=you/your-repo
Environment=BRANCH=main
Environment=WORK_DIR=/var/lib/poll-ci
ExecStart=/usr/local/bin/poll-ci
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now poll-ci
journalctl -u poll-ci -f
```

### Updating

```bash
docker compose pull && docker compose up -d   # pull the newest image and recreate
# not using compose?  docker pull ghcr.io/extremeshok/poll-ci && \
#   docker rm -f poll-ci && docker run …   (the run command from the quickstart)
```

State on the volume is preserved, so already-tested commits aren't re-run.

**Automatic updates (optional).** The bundled compose file ships a profile that
keeps poll-ci on the latest image for you, via [watchtower]:

```bash
docker compose --profile autoupdate up -d
```

Watchtower polls the registry hourly and recreates the poll-ci container in place
when its image digest changes (scoped by label to only touch poll-ci). A new
image landing mid-check is safe: poll-ci catches SIGTERM, cancels the in-flight
check, and re-runs that commit after the restart. If the branch moved on while
it was down, the interrupted commit's statuses are resolved at startup
(`interrupted by restart`) instead of lingering `pending`.

[watchtower]: https://containrrr.dev/watchtower/

---

## Token setup

poll-ci needs to **read commits** and **write statuses** — nothing else.

### Fine-grained PAT (recommended)

GitHub → *Settings → Developer settings → Fine-grained tokens → Generate new
token*:

1. **Repository access** → select the repos you want to watch.
2. **Permissions → Repository permissions**:
   - **Contents: Read-only**  (read the branch HEAD and clone) — bump to
     **Read and write** only if you use `promote:` (to fast-forward the deploy branch)
   - **Commit statuses: Read and write**  (post results)
3. Generate, copy, and pass it as `GITHUB_TOKEN`.

### Classic PAT

A classic token with the **`repo`** scope also works (it's broader than
needed). Fine for private repos; use the fine-grained token to stay
least-privilege.

The token is sent as an HTTP auth header for git, passed via environment
variables (`GIT_CONFIG_*`, needs git ≥ 2.31 when running the plain binary) so
it is **invisible in process listings** and **never written into
`.git/config`** — it can't leak into your check containers — and both the raw
token and its base64 header form are scrubbed from logs.

---

## Required status checks

Make poll-ci's results gate merges:

1. Repo → *Settings → Branches → Add branch protection rule*.
2. Enable **Require status checks to pass before merging**.
3. In the search box, pick `ci` (the rollup) and/or individual `ci/<name>`
   contexts.

> The contexts only appear in that list **after poll-ci has posted them at
> least once** — push a commit first, let poll-ci run, then add the rule.

---

## Operating poll-ci

**Watch logs** (engine logs go to stderr; live check output to stdout — both via
`docker logs`):

```bash
docker logs -f poll-ci
```

**Inspect what's been tested:**

```bash
docker exec poll-ci cat /var/lib/poll-ci/state.json
```

**Re-run a commit** (e.g. after fixing a flaky check) — `--once` ignores state:

```bash
docker run --rm -e GITHUB_TOKEN=… -e REPO=you/your-repo \
  -v /var/run/docker.sock:/var/run/docker.sock \
  poll-ci --once <sha>
```

**Reset all state** (re-test the current HEAD of every watched branch):

```bash
docker rm -f poll-ci
docker run --rm -v poll-ci-state:/s alpine rm -f /s/state.json
# then start poll-ci again
```

**Watch open pull requests too** — set `POLL_PRS=true`. poll-ci will also test
the head commit of each open PR **whose branch lives in the same repo**. PRs
from forks are skipped on purpose (see [Security](#security)).

**Startup housekeeping.** On every start poll-ci removes check containers and
checkout dirs orphaned by a previous crash (scoped by an instance label, so
co-located poll-ci instances on a shared daemon never touch each other), and
resolves any statuses a killed run left `pending` on GitHub. Containers from
pre-v1.4 versions carry only the generic label — prune those once by hand:
`docker ps -aq -f label=poll-ci | xargs docker rm -f`.

---

## Security

poll-ci is small, but it runs commands from your repos in Docker. Read this.

- **The Docker socket is root on the host.** `-v /var/run/docker.sock:…` lets
  poll-ci — and therefore every check command — control the host's Docker
  daemon, which is effectively root on that machine. **Run poll-ci on a
  dedicated CI host or VM**, not on a box with anything precious on it. This is
  the standard trade-off for socket-based "sibling container" CI.
- **Checks run arbitrary code from the repo.** Anyone who can push to a watched
  branch can run commands on your CI host. Only watch repos and branches you
  trust, and protect those branches.
- **PR polling runs contributor code.** With `POLL_PRS=true`, poll-ci tests open
  PRs — but **only those whose head branch is in the same repo**. Fork PRs are
  **skipped on purpose**, because running untrusted fork code next to the Docker
  socket is dangerous. Even so, enable it only where branch-push is restricted
  to people you trust.
- **Use a least-privilege token** — a fine-grained PAT scoped to just the
  watched repos, with only Contents:read + Commit statuses:write.
- **No secret store, by design.** poll-ci has no vault. Don't put secrets in
  `.poll-ci.yml`. If a check needs credentials, bake them into a private
  `image:` you control.

Socket-free alternative: if you won't expose the socket, bake your toolchain
into the engine image and run checks in-process — but the documented, simple
path is the mounted socket above.

---

## Troubleshooting

**Statuses never appear on GitHub.**
Check `docker logs poll-ci`. Common causes: wrong/expired `GITHUB_TOKEN`, the
token lacks **Commit statuses: write**, or the token can't see a private repo.
A `403`/`404` in the logs on `set status` points at token permissions.

**Log says `no .poll-ci.yml at repo root`.**
The file must be at the repository root on the watched branch, named
`.poll-ci.yml` or `.poll-ci.yaml`. poll-ci posts a single red `ci` status saying
this, so it's visible on the commit too.

**A check fails with `exit 127: sh: <tool>: not found`.**
The command isn't available in the `image:` you chose. Pick an image that has
the tool (e.g. `golang:1.26`, `node:22`), or install it inside the `run`.

**`docker: command not found` or socket permission denied.**
The engine container needs the Docker socket mounted
(`-v /var/run/docker.sock:/var/run/docker.sock`). Running the binary directly?
The host user must be able to run `docker` (in the `docker` group).

**`branch "main" not found`.**
Wrong `BRANCH`, or the repo's default branch is `master`/something else. Set
`BRANCH` to match.

**Image pulls fail with Docker Hub rate limits.**
Unauthenticated Docker Hub pulls are rate-limited. `docker login` on the CI
host, or use images from a registry/mirror you control.

**It only tested the latest commit, skipping ones in between.**
By design — poll-ci tests the current **HEAD** of each branch per poll. If you
push 3 commits between polls, it tests the tip. Lower `POLL_INTERVAL` to catch
more, but it never tests every intermediate commit of a fast push. Set
`MARK_SKIPPED=true` to at least stamp the skipped commits with a terminal
status instead of leaving them blank.

**A commit is stuck on a yellow "pending" status.**
Shouldn't happen anymore: status posts are retried, and statuses stranded by a
crash/restart are resolved at the next startup. If you still see one (e.g.
posted by an old version), re-run the commit with `--once`.

**It re-ran a commit after I recreated the container.**
You didn't mount the state volume (`-v poll-ci-state:/var/lib/poll-ci`). Without
it, state is lost on recreate and the current HEAD is re-tested once.

**GitHub Enterprise Server.**
Set `GITHUB_API=https://ghe.example.com/api/v3` and `GIT_HOST=ghe.example.com`.

---

## Limitations

Stated plainly, so there are no surprises:

- **Polling latency.** A push is noticed within `POLL_INTERVAL` seconds, not
  instantly.
- **Tests HEAD only.** Each poll tests the branch tip; intermediate commits of a
  multi-commit push aren't individually tested (`MARK_SKIPPED` at least makes
  that visible on each skipped commit).
- **Parallel checks (default).** A repo's checks run up to `CHECK_CONCURRENCY`
  (default: the host CPU count) at a time, bounded by `CHECK_CPU_BUDGET`;
  `CHECK_CONCURRENCY=1` restores one-at-a-time. Repos are still swept one at a time
  by default — set `CONCURRENCY=N` to sweep several in parallel. Under parallelism
  each check's output is buffered into a labeled block (serial keeps live streaming).
- **Dependency caching (opt-in).** With `CACHE=true`, a clean, auto-detected
  dependency cache is copied into each check (`cache:` customizes). Off by default
  because the copy-in only pays off for download-heavy checks ([details](#caching-dependencies-cache)).
  Docker *image* layers are cached by the host daemon regardless.
- **One image per repo**, shared by all its checks.
- **No secrets management, no artifacts, no logs UI.** Output lives in
  `docker logs`; the status description is a one-line summary.
- **Same-repo PRs only** when `POLL_PRS` is on; forks are skipped for safety.

These are deliberate. If you've outgrown them, that's the signal to move to a
full CI (Woodpecker, Drone, Actions).

---

## FAQ

**Why polling instead of webhooks?**
So poll-ci needs **no inbound ports and no public endpoint** — only outbound
HTTPS to GitHub. That's what makes it work behind NAT, on a laptop, or inside a
locked-down network. The cost is up to `POLL_INTERVAL` of latency.

**Why not Woodpecker / Drone / Jenkins?**
They're great and much bigger: server + agents + web UI + database + inbound
webhooks. poll-ci is the **minimal opposite** — one binary in one container,
outbound-only. Different tool for a different moment.

**Does it survive restarts and reboots?**
Yes. The tested-SHA set is persisted to the state volume and `--restart always`
brings the container back after a reboot. Already-tested commits aren't re-run.

**Can I run checks in parallel / cache dependencies?**
As of v1.5, **parallel checks are on by default** (up to `CHECK_CONCURRENCY`, the
host CPU count) — `CHECK_CONCURRENCY=1` restores the old one-at-a-time behavior.
**Dependency caching is opt-in** (`CACHE=true`): it copies a clean, auto-detected
golden cache into each check, which helps **download-heavy** checks but not
compute-bound ones — so measure before enabling (see
[`cache:`](#caching-dependencies-cache)). (*Repos* are still swept one at a time by
default — `CONCURRENCY=N` sweeps several at once.)

**Does it work with private repos?**
Yes — give the token access to them.

**How do I install without Docker?**
`go install github.com/extremeshok/poll-ci@latest` (installs to
`$(go env GOPATH)/bin`), then run the binary on a host that has `git` and
`docker`. Same environment variables. See [Deploying](#deploying) for a systemd
unit.

---

## Building from source

```bash
git clone https://github.com/extremeshok/poll-ci && cd poll-ci
go build -o poll-ci .          # local binary (needs git + docker on PATH to run)
go test ./...                  # unit tests
docker build -t poll-ci .      # engine image (~57 MB: Alpine + git + Docker CLI)
```

The source is intentionally small — `main.go`, `config.go`, `github.go`,
`runner.go`, `state.go`, `util.go`. Start at
[`runner.go`](runner.go) to see the whole lifecycle.

## License

[MIT](LICENSE)
