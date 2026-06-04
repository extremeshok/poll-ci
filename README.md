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

![license](https://img.shields.io/badge/license-MIT-blue)
![go](https://img.shields.io/badge/go-1.26-00ADD8)
![deps](https://img.shields.io/badge/dependencies-1-brightgreen)
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
   then an overall `ci` rollup (`success` only if every check passed).
7. **Remember** — records the SHA so it never runs twice, and survives restarts.

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
image: alpine:3.20
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
```

**Rules and behavior:**

- Each check becomes a commit status `ci/<name>`. An overall `ci` status rolls
  them up (`success` only if **every** check passed).
- A check **passes if its command exits `0`**. On failure the status
  description is the last line of output (stderr preferred) — e.g.
  `exit 1: FAIL: expected 4 but got 5`. A check that exceeds its `timeout` is a
  failure (`timed out after 15m0s`) and its container is killed.
- Checks run **sequentially, each in its own fresh container** built from
  `image`, with the commit's files at `/repo` (the working directory). `.git`
  is included too, but it's a **shallow, depth-1 clone** — full history and tags
  aren't fetched, so `git describe --tags` and `git log` beyond the tip won't
  work.
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
image: alpine:3.20
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
| `GITHUB_API`      | `https://api.github.com` | API base — set for GitHub Enterprise Server                   |
| `GIT_HOST`        | `github.com`             | Git host for clones — set for GHES                            |
| `DOCKER_BIN`      | `docker`                 | Docker CLI to invoke                                          |

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

### docker run

The [Quickstart](#quickstart-60-seconds) command, with `--restart always`, is a
complete deployment. It survives reboots (Docker restarts the container) and
restarts (the state volume means tested commits aren't re-run).

### docker compose

Use the included [`docker-compose.yml`](docker-compose.yml):

```bash
export GITHUB_TOKEN=github_pat_xxx
cp repos.example.yml repos.yml      # then edit it
docker compose up -d --build
```

### As a plain binary (systemd)

poll-ci is a single binary; it shells out to `git` and `docker`, both of which
must be on `PATH` (and the user must be able to run `docker`). Build and install
it directly if you'd rather not containerize the engine:

```bash
git clone https://github.com/extremeshok/poll-ci && cd poll-ci
go build -o poll-ci . && sudo install poll-ci /usr/local/bin/
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
cd poll-ci && git pull
docker compose up -d --build      # or: docker build -t poll-ci . && docker rm -f poll-ci && docker run ...
```

State on the volume is preserved, so already-tested commits aren't re-run.

---

## Token setup

poll-ci needs to **read commits** and **write statuses** — nothing else.

### Fine-grained PAT (recommended)

GitHub → *Settings → Developer settings → Fine-grained tokens → Generate new
token*:

1. **Repository access** → select the repos you want to watch.
2. **Permissions → Repository permissions**:
   - **Contents: Read-only**  (read the branch HEAD and clone)
   - **Commit statuses: Read and write**  (post results)
3. Generate, copy, and pass it as `GITHUB_TOKEN`.

### Classic PAT

A classic token with the **`repo`** scope also works (it's broader than
needed). Fine for private repos; use the fine-grained token to stay
least-privilege.

The token is sent as an HTTP auth header for git — it's **never written into
`.git/config`**, so it can't leak into your check containers — and it's
scrubbed from logs.

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
more, but it never tests every intermediate commit of a fast push.

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
  multi-commit push aren't individually tested.
- **Sequential.** Checks and repos run one at a time. No parallelism — per-check
  `timeout` keeps a stuck check from blocking forever.
- **No caching.** Every run starts clean (Docker *image* layers are still cached
  by the host daemon). Warm dependencies? Bake them into a custom `image:`.
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
No — both are intentionally left out to keep the tool tiny and predictable. See
[Limitations](#limitations). Bake heavy dependencies into a custom `image:`.

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
