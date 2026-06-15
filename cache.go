package main

// cache.go — opt-out dependency caching. A clean "golden" cache is built once by
// an ecosystem-detected (or repo-declared) prime command in an isolated
// container, then copied into every check container. Checks never write the
// golden back, so it cannot be poisoned by a run or a PR; it only ever makes
// runs faster, never changes their result (a check must still pass from cold).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ecoPreset is one ecosystem's cache rule: the marker file that identifies a
// module, the global cache paths to persist, and the command that populates them
// from inside a module directory.
type ecoPreset struct {
	marker string
	paths  []string
	cmd    string
}

// ecoPresets maps common ecosystems to cache presets. Detection is
// monorepo-aware (detectCachePlan walks the tree), so a repo with several
// modules — or a polyglot repo — gets every module primed, not just the root.
// Commands assume the repo's image has the toolchain; override with an explicit
// cache: block when a preset doesn't fit.
var ecoPresets = []ecoPreset{
	{"go.mod", []string{"/go/pkg/mod", "/root/.cache/go-build"}, "go mod download"},
	{"package-lock.json", []string{"/root/.npm"}, "npm ci"},
	{"yarn.lock", []string{"/usr/local/share/.cache/yarn", "/root/.cache/yarn"}, "yarn install --frozen-lockfile"},
	{"pnpm-lock.yaml", []string{"/root/.local/share/pnpm/store"}, "pnpm install --frozen-lockfile"},
	{"requirements.txt", []string{"/root/.cache/pip"}, "pip download -r requirements.txt -d /tmp/poll-ci-pip"},
	{"poetry.lock", []string{"/root/.cache/pypoetry", "/root/.cache/pip"}, "poetry install --no-root --no-interaction"},
	{"Cargo.lock", []string{"/root/.cargo/registry", "/root/.cargo/git"}, "cargo fetch"},
}

// cachePlan is the resolved caching for a run. Empty paths ⇒ no caching.
type cachePlan struct {
	paths   []string
	prime   string
	markers []string
}

// cacheMount is a golden host directory to copy into a check container at a path.
type cacheMount struct {
	hostDir       string
	containerPath string
}

// resolveCache decides what (if anything) to cache for this run: global CACHE
// off or `cache: false` ⇒ nothing; explicit `cache:` ⇒ as declared; otherwise
// auto-detect by ecosystem marker files in the checkout.
func (r *Runner) resolveCache(rc *RepoConfig, dir string) cachePlan {
	if !r.cfg.CacheEnabled {
		return cachePlan{}
	}
	if rc.Cache != nil && rc.Cache.Disabled {
		return cachePlan{}
	}
	if rc.Cache != nil && rc.Cache.Explicit {
		return cachePlan{paths: dedupe(rc.Cache.Paths), prime: rc.Cache.Prime}
	}
	return detectCachePlan(dir)
}

// detectCachePlan walks the checkout (bounded depth, skipping vcs/vendor/deps
// directories) for ecosystem markers and builds a plan that primes EVERY module
// found — so a monorepo's nested Go modules and JS workspaces are all warmed, not
// just the repo root. The prime runs each module's populate command in its own
// directory; one module's failure (|| true) doesn't abandon the rest.
func detectCachePlan(dir string) cachePlan {
	const maxDepth = 4
	idx := make(map[string]int, len(ecoPresets))
	for i, p := range ecoPresets {
		idx[p.marker] = i
	}
	dirsFor := make([][]string, len(ecoPresets))
	var plan cachePlan
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			if rel == "." {
				return nil
			}
			switch d.Name() {
			case ".git", "vendor", "node_modules", "testdata":
				return filepath.SkipDir
			}
			if strings.Count(rel, string(filepath.Separator))+1 > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if i, ok := idx[d.Name()]; ok {
			dirsFor[i] = append(dirsFor[i], filepath.Dir(rel))
			plan.markers = append(plan.markers, rel)
		}
		return nil
	})
	seen := map[string]bool{}
	var primes []string
	for i, p := range ecoPresets {
		if len(dirsFor[i]) == 0 {
			continue
		}
		for _, path := range p.paths {
			if !seen[path] {
				seen[path] = true
				plan.paths = append(plan.paths, path)
			}
		}
		for _, rd := range dirsFor[i] {
			if rd == "." {
				primes = append(primes, p.cmd+" || true")
			} else {
				primes = append(primes, "(cd "+shQuote(rd)+" && "+p.cmd+") || true")
			}
		}
	}
	plan.prime = strings.Join(primes, "; ")
	return plan
}

// shQuote single-quotes s for safe inclusion in a /bin/sh command.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// cacheMeta records what the golden cache was primed from, so it can be
// re-primed exactly when dependencies change. Stored beside the cache.
type cacheMeta struct {
	LockHash string `json:"lock_hash"`
	Primed   int64  `json:"primed_unix"`
}

// prepareCache makes the golden cache for this run current (re-priming when this
// is a prime-eligible branch run and the lockfiles changed or the refresh window
// lapsed) and returns the host dirs to copy into each check container. It is
// best-effort: any failure logs and falls back to whatever golden exists (the
// affected checks just run colder), never failing the run.
func (r *Runner) prepareCache(ctx context.Context, ref Ref, image, dir string, plan cachePlan, canPrime bool) []cacheMount {
	if len(plan.paths) == 0 {
		return nil
	}
	root := filepath.Join(r.cfg.WorkDir, "cache", cacheKey(ref.Repo(), ref.Branch))
	lk := r.cacheLock(root)
	lk.Lock()
	defer lk.Unlock()

	if canPrime && plan.prime != "" && r.cacheNeedsPrime(dir, root, plan) {
		log.Printf("[%s] priming cache: %s", ref, strings.Join(plan.paths, ", "))
		if err := r.primeCache(ctx, image, dir, root, plan); err != nil {
			log.Printf("[%s] cache prime failed (continuing without it): %v", ref, oneLine(scrub(err.Error(), r.secrets...)))
		} else {
			r.writeCacheMeta(root, dir, plan)
		}
	}

	var mounts []cacheMount
	for _, p := range plan.paths {
		gdir := cachePathDir(root, p)
		if fi, err := os.Stat(gdir); err == nil && fi.IsDir() {
			mounts = append(mounts, cacheMount{hostDir: gdir, containerPath: p})
		}
	}
	return mounts
}

// cacheNeedsPrime reports whether the golden cache must be (re)built: any path
// missing, no recorded metadata, the lockfiles changed since the last prime, or
// the refresh window lapsed.
func (r *Runner) cacheNeedsPrime(dir, root string, plan cachePlan) bool {
	for _, p := range plan.paths {
		if _, err := os.Stat(cachePathDir(root, p)); err != nil {
			return true
		}
	}
	meta := r.readCacheMeta(root)
	if meta == nil {
		return true
	}
	if h := lockfileHash(dir, plan.markers); h != "" && h != meta.LockHash {
		return true
	}
	if r.cfg.CacheRefresh > 0 && time.Since(time.Unix(meta.Primed, 0)) >= r.cfg.CacheRefresh {
		return true
	}
	return false
}

// primeCache runs the plan's prime command in a clean container (seeded with any
// existing golden so it updates incrementally rather than re-downloading), then
// harvests each cache path out into its golden dir with an atomic swap.
func (r *Runner) primeCache(ctx context.Context, image, dir, root string, plan cachePlan) error {
	cctx, cancel := context.WithTimeout(ctx, r.cfg.DefaultTimeout)
	defer cancel()

	cid, err := r.dockerCreate(cctx, image, plan.prime, "", r.cfg.CheckMemory)
	if err != nil {
		return fmt.Errorf("create: %v", err)
	}
	defer r.dockerRemove(cid)

	if err := r.dockerCopyIn(cctx, dir, cid); err != nil {
		return fmt.Errorf("copy sources: %v", err)
	}
	for _, p := range plan.paths {
		gdir := cachePathDir(root, p)
		if fi, err := os.Stat(gdir); err == nil && fi.IsDir() {
			if err := r.dockerCopyDirIn(cctx, gdir, cid, p); err != nil {
				log.Printf("cache: seed %s: %v", p, oneLine(err.Error()))
			}
		}
	}

	_, _, startErr := r.dockerStart(cctx, cid, os.Stdout)
	code, err := r.dockerOutcome(cctx, cid, startErr)
	if err != nil {
		return fmt.Errorf("run: %v", err)
	}
	if code != 0 {
		return fmt.Errorf("prime command exited %d", code)
	}

	for _, p := range plan.paths {
		if err := r.harvestCachePath(cctx, cid, p, root); err != nil {
			log.Printf("cache: harvest %s: %v", p, oneLine(err.Error()))
		}
	}
	return nil
}

// harvestCachePath copies one cache path out of the prime container into its
// golden dir, swapping atomically so a concurrent copy-in never sees a torn dir.
func (r *Runner) harvestCachePath(ctx context.Context, cid, containerPath, root string) error {
	golden := cachePathDir(root, containerPath)
	tmp := golden + ".tmp"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	if _, errb, err := r.dockerRun(ctx, "cp", cid+":"+containerPath+"/.", tmp); err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("%v: %s", err, oneLine(errb))
	}
	old := golden + ".old"
	os.RemoveAll(old)
	if _, err := os.Stat(golden); err == nil {
		if err := os.Rename(golden, old); err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, golden); err != nil {
		os.Rename(old, golden) // best-effort restore
		os.RemoveAll(tmp)
		return err
	}
	os.RemoveAll(old)
	return nil
}

// dockerCopyDirIn streams a host directory into a container at containerPath via
// `docker cp -` (a tar extracted at /), mirroring dockerCopyIn for the checkout.
func (r *Runner) dockerCopyDirIn(ctx context.Context, hostDir, cid, containerPath string) error {
	cmd := exec.CommandContext(ctx, r.cfg.DockerBin, "cp", "-", cid+":/")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return err
	}
	// Names are prefixed with the (slash-stripped) container path, so extracting
	// at "/" lands them at containerPath and creates intermediate dirs.
	tarErr := writeTar(stdin, hostDir, strings.TrimPrefix(containerPath, "/"))
	stdin.Close()
	waitErr := cmd.Wait()
	if tarErr != nil {
		return tarErr
	}
	if waitErr != nil {
		return fmt.Errorf("%v: %s", waitErr, oneLine(errb.String()))
	}
	return nil
}

func (r *Runner) readCacheMeta(root string) *cacheMeta {
	data, err := os.ReadFile(filepath.Join(root, "meta.json"))
	if err != nil {
		return nil
	}
	var m cacheMeta
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return &m
}

func (r *Runner) writeCacheMeta(root, dir string, plan cachePlan) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		log.Printf("cache: meta dir %s: %v", root, err)
		return
	}
	m := cacheMeta{LockHash: lockfileHash(dir, plan.markers), Primed: time.Now().Unix()}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(root, "meta.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("cache: meta write: %v", err)
		return
	}
	os.Rename(tmp, filepath.Join(root, "meta.json"))
}

// cacheLock returns the per-cache-key mutex, serializing prime/harvest so two
// runs sharing a golden never write it at once.
func (r *Runner) cacheLock(key string) *sync.Mutex {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	m := r.cacheLocks[key]
	if m == nil {
		m = &sync.Mutex{}
		r.cacheLocks[key] = m
	}
	return m
}
