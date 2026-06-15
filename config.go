package main

// config.go — all configuration: the runner's environment config, the optional
// multi-repo list (repos.yml), and the per-repo .poll-ci.yml that lives in each
// watched repository. Deliberately flat: no stages, no matrices, no plugins.

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Ref is a single watched target: a repository and a branch.
type Ref struct {
	Owner  string
	Name   string
	Branch string
}

// Repo returns "owner/name".
func (r Ref) Repo() string { return r.Owner + "/" + r.Name }

// String returns "owner/name@branch" for logging.
func (r Ref) String() string { return r.Repo() + "@" + r.Branch }

// RunnerConfig is the engine configuration, read entirely from the environment.
type RunnerConfig struct {
	Token          string        // GITHUB_TOKEN (required)
	APIBase        string        // GITHUB_API (default https://api.github.com)
	GitHost        string        // GIT_HOST  (default github.com)
	Refs           []Ref         // from REPO+BRANCH or REPOS_FILE
	PollInterval   time.Duration // POLL_INTERVAL seconds (default 60)
	WorkDir        string        // WORK_DIR (scratch clones + state)
	StateFile      string        // STATE_FILE (default WORK_DIR/state.json)
	DefaultImage   string        // DEFAULT_IMAGE for repos that omit image:
	DefaultTimeout time.Duration // DEFAULT_TIMEOUT seconds per check (default 1800)
	DockerBin      string        // DOCKER_BIN (default "docker")
	PollPRs        bool          // POLL_PRS — also test same-repo open PR heads
	ShortCircuit   bool          // SHORT_CIRCUIT — abort a stale run when a newer tip appears (default true)
	MarkSkipped    bool          // MARK_SKIPPED — post a terminal `ci` status on never-tested intermediate commits
	CheckMemory    string        // CHECK_MEMORY — docker --memory for check/scan containers (e.g. "2g"; empty = unlimited)
	CheckCPUs      string        // CHECK_CPUS — docker --cpus (e.g. "2"; empty = unlimited)
	CheckPids      string        // CHECK_PIDS — docker --pids-limit (e.g. "4096"; empty = unlimited)
	ImageRefresh   time.Duration // IMAGE_REFRESH_HOURS — re-pull present images this often (0 = never, the default)
	Concurrency    int           // CONCURRENCY — refs swept in parallel (default 1 = serial)

	CheckConcurrency int           // CHECK_CONCURRENCY — a commit's checks run in parallel up to this many (default: host CPU count; 1 = serial)
	CheckCPUBudget   float64       // CHECK_CPU_BUDGET — total --cpus admitted at once across a commit's checks (default NumCPU)
	CacheEnabled     bool          // CACHE — opt-in clean dependency-cache copy-in (default false; CACHE=true enables)
	CacheRefresh     time.Duration // CACHE_REFRESH_HOURS — re-prime the golden cache at least this often (default 24h; 0 = only when missing/stale)
}

// RepoConfig is a repository's .poll-ci.yml.
type RepoConfig struct {
	Image   string   `yaml:"image"`
	Checks  []Check  `yaml:"checks"`
	Promote *Promote `yaml:"promote"`
	Scan    *Scan    `yaml:"scan"`
	Cache   *Cache   `yaml:"cache"`
}

// Cache configures dependency caching for a repo. Three forms in .poll-ci.yml:
//
//	(absent)                         → auto-detect from ecosystem marker files
//	cache: false                     → disabled for this repo
//	cache: {paths: […], prime: "…"}  → explicit cache paths + prime command
//
// A clean "golden" cache is built once by the prime command in an isolated
// container and then copied into each check container; checks never write it
// back, so it cannot be poisoned by a run (or by a PR). It only speeds runs up —
// a check must still pass from a cold cache.
type Cache struct {
	Disabled bool     // cache: false
	Explicit bool     // an explicit mapping was given (skip auto-detection)
	Paths    []string // container paths to persist (absolute)
	Prime    string   // command run in a clean container to populate the cache
}

// UnmarshalYAML accepts either `cache: false` (a scalar bool) or an explicit
// `cache: {paths, prime}` mapping; absence leaves RepoConfig.Cache nil, which
// the runner treats as "auto-detect".
func (c *Cache) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var b bool
		if err := value.Decode(&b); err != nil {
			return fmt.Errorf("cache: want `false` or a mapping {paths, prime}")
		}
		c.Disabled = !b
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("cache: want `false` or a mapping {paths, prime}")
	}
	var r struct {
		Paths []string `yaml:"paths"`
		Prime string   `yaml:"prime"`
	}
	if err := value.Decode(&r); err != nil {
		return fmt.Errorf("invalid cache: %w", err)
	}
	c.Explicit = true
	c.Paths = r.Paths
	c.Prime = strings.TrimSpace(r.Prime)
	return nil
}

// Default trivy image + cache volume for the built-in scan: step.
const (
	DefaultTrivyImage       = "aquasec/trivy:latest"
	DefaultTrivyCacheVolume = "poll-ci-trivy-cache"
	DefaultTrivyScanners    = "vuln,secret,misconfig"
	DefaultTrivySeverity    = "HIGH,CRITICAL"
)

// Check is one named command to run.
type Check struct {
	Name    string `yaml:"name"`
	Run     string `yaml:"run"`
	Timeout int    `yaml:"timeout"` // seconds; 0 → DefaultTimeout
	CPUs    string `yaml:"cpus"`    // optional docker --cpus, also the check's scheduler cost (empty → 1.0 when parallel)
	Memory  string `yaml:"memory"`  // optional docker --memory cap (empty → CHECK_MEMORY)
}

// cpuCost is the check's scheduler cost and docker --cpus value: its cpus: hint,
// or 1.0 when unset. parseRepoConfig validates that a set hint parses positive.
func (ck Check) cpuCost() float64 {
	if ck.CPUs == "" {
		return 1
	}
	n, err := strconv.ParseFloat(ck.CPUs, 64)
	if err != nil || n <= 0 {
		return 1
	}
	return n
}

// Promote optionally fast-forwards a target branch to the tested commit when
// every check passes on a watched branch (never on PRs). It lets a deploy branch
// be gated behind green CI without GitHub Actions: e.g. watch `master`, and on
// green fast-forward `release`, which a deploy poller then ships.
//
// This needs the GITHUB_TOKEN to have "Contents: write". Status reporting alone
// only needs "Commit statuses: write" + "Contents: read", so promotion is opt-in
// and the extra scope is only required when a repo's .poll-ci.yml requests it.
type Promote struct {
	Branch string `yaml:"branch"` // target branch to fast-forward, e.g. "release"
}

// Scan optionally runs a trivy filesystem scan of the checkout as a built-in
// gate step, reported as a `ci/trivy` commit status. A finding fails the gate —
// so, combined with promote:, vulnerable/secret-leaking/misconfigured source
// never advances a deploy branch. This is shift-left supply-chain scanning of
// the SOURCE (dependency manifests, leaked secrets, Dockerfile/IaC misconfig),
// complementary to scanning built images at deploy time. trivy needs no GitHub
// token. It runs via trivy's own entrypoint (argv passing — no shell), and its
// vulnerability DB is cached in a named Docker volume so it isn't re-downloaded
// every run.
//
// Minimal form is just `scan: {}` — every field defaults.
type Scan struct {
	Image         string `yaml:"image"`          // trivy image (default aquasec/trivy:latest)
	Scanners      string `yaml:"scanners"`       // trivy --scanners (default vuln,secret,misconfig)
	Severity      string `yaml:"severity"`       // trivy --severity (default HIGH,CRITICAL)
	IgnoreUnfixed *bool  `yaml:"ignore-unfixed"` // trivy --ignore-unfixed (default true)
	Path          string `yaml:"path"`           // sub-path under the repo to scan (default ".")
	Timeout       int    `yaml:"timeout"`        // seconds; 0 → DefaultTimeout
	CacheVolume   string `yaml:"cache-volume"`   // named volume for the trivy DB cache
}

// LoadRunnerConfig builds the engine configuration from environment variables.
func LoadRunnerConfig() (*RunnerConfig, error) {
	c := &RunnerConfig{
		Token:          os.Getenv("GITHUB_TOKEN"),
		APIBase:        envOr("GITHUB_API", "https://api.github.com"),
		GitHost:        envOr("GIT_HOST", "github.com"),
		WorkDir:        os.Getenv("WORK_DIR"),
		StateFile:      os.Getenv("STATE_FILE"),
		DefaultImage:   envOr("DEFAULT_IMAGE", "alpine:latest"),
		DockerBin:      envOr("DOCKER_BIN", "docker"),
		PollPRs:        truthy(os.Getenv("POLL_PRS")),
		ShortCircuit:   boolEnv("SHORT_CIRCUIT", true),
		MarkSkipped:    truthy(os.Getenv("MARK_SKIPPED")),
		CheckMemory:    os.Getenv("CHECK_MEMORY"),
		CheckCPUs:      os.Getenv("CHECK_CPUS"),
		CheckPids:      os.Getenv("CHECK_PIDS"),
		PollInterval:   secondsOr("POLL_INTERVAL", 60),
		DefaultTimeout: secondsOr("DEFAULT_TIMEOUT", 1800),
		ImageRefresh:   time.Duration(intEnv("IMAGE_REFRESH_HOURS", 0)) * time.Hour,
		Concurrency:    intEnv("CONCURRENCY", 1),

		CheckConcurrency: intEnv("CHECK_CONCURRENCY", runtime.NumCPU()),
		CheckCPUBudget:   floatEnv("CHECK_CPU_BUDGET", float64(runtime.NumCPU())),
		CacheEnabled:     boolEnv("CACHE", false),
		CacheRefresh:     time.Duration(intEnv("CACHE_REFRESH_HOURS", 24)) * time.Hour,
	}
	if c.Concurrency < 1 {
		c.Concurrency = 1
	}
	if c.CheckConcurrency < 1 {
		c.CheckConcurrency = 1
	}
	if c.Token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is required")
	}
	// These become docker argv; same conservative charset as the scan: fields.
	for _, f := range []struct{ name, val string }{
		{"CHECK_MEMORY", c.CheckMemory}, {"CHECK_CPUS", c.CheckCPUs}, {"CHECK_PIDS", c.CheckPids},
	} {
		if f.val != "" && !safeArg(f.val) {
			return nil, fmt.Errorf("%s contains unsupported characters: %q", f.name, f.val)
		}
	}
	c.APIBase = strings.TrimRight(c.APIBase, "/")

	// Resolve a writable work directory: explicit WORK_DIR, else /var/lib/poll-ci,
	// else a temp dir (handy when running the binary directly on a dev machine).
	if c.WorkDir == "" {
		for _, cand := range []string{"/var/lib/poll-ci", os.TempDir() + "/poll-ci"} {
			if os.MkdirAll(cand, 0o755) == nil {
				c.WorkDir = cand
				break
			}
		}
		if c.WorkDir == "" {
			return nil, fmt.Errorf("could not create a work directory; set WORK_DIR")
		}
	} else if err := os.MkdirAll(c.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("WORK_DIR %q: %w", c.WorkDir, err)
	}
	if c.StateFile == "" {
		c.StateFile = c.WorkDir + "/state.json"
	}

	// Targets: either a single REPO+BRANCH, or a REPOS_FILE listing several.
	refs, err := loadRefs()
	if err != nil {
		return nil, err
	}
	c.Refs = refs
	return c, nil
}

// loadRefs reads REPO/BRANCH or REPOS_FILE into a list of Refs.
func loadRefs() ([]Ref, error) {
	if path := os.Getenv("REPOS_FILE"); path != "" {
		return loadReposFile(path)
	}
	repo := os.Getenv("REPO")
	if repo == "" {
		return nil, fmt.Errorf("set REPO=owner/name (and optional BRANCH) or REPOS_FILE=/path/to/repos.yml")
	}
	ref, err := parseRef(repo, envOr("BRANCH", "main"))
	if err != nil {
		return nil, err
	}
	return []Ref{ref}, nil
}

// loadReposFile parses a repos.yml of the form:
//
//	repos:
//	  - repo: owner/name
//	    branch: main
func loadReposFile(path string) ([]Ref, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read REPOS_FILE: %w", err)
	}
	var f struct {
		Repos []struct {
			Repo   string `yaml:"repo"`
			Branch string `yaml:"branch"`
		} `yaml:"repos"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse REPOS_FILE: %w", err)
	}
	if len(f.Repos) == 0 {
		return nil, fmt.Errorf("REPOS_FILE %q lists no repos", path)
	}
	var refs []Ref
	for _, r := range f.Repos {
		branch := r.Branch
		if branch == "" {
			branch = "main"
		}
		ref, err := parseRef(r.Repo, branch)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// parseRepoConfig parses a .poll-ci.yml byte slice and validates it.
func parseRepoConfig(data []byte) (*RepoConfig, error) {
	var rc RepoConfig
	if err := yaml.Unmarshal(data, &rc); err != nil {
		return nil, fmt.Errorf("invalid .poll-ci.yml: %w", err)
	}
	if len(rc.Checks) == 0 {
		return nil, fmt.Errorf(".poll-ci.yml defines no checks")
	}
	seen := map[string]bool{}
	for i, ck := range rc.Checks {
		if ck.Name == "" {
			return nil, fmt.Errorf("check #%d has no name", i+1)
		}
		if ck.Run == "" {
			return nil, fmt.Errorf("check %q has no run command", ck.Name)
		}
		if seen[ck.Name] {
			return nil, fmt.Errorf("duplicate check name %q", ck.Name)
		}
		seen[ck.Name] = true
		// cpus/memory become docker argv; validate the same conservative charset
		// as the CHECK_* limits, and that a cpus hint is a positive number.
		if ck.CPUs != "" {
			if !safeArg(ck.CPUs) {
				return nil, fmt.Errorf("check %q cpus contains unsupported characters: %q", ck.Name, ck.CPUs)
			}
			if n, err := strconv.ParseFloat(ck.CPUs, 64); err != nil || n <= 0 {
				return nil, fmt.Errorf("check %q cpus must be a positive number: %q", ck.Name, ck.CPUs)
			}
		}
		if ck.Memory != "" && !safeArg(ck.Memory) {
			return nil, fmt.Errorf("check %q memory contains unsupported characters: %q", ck.Name, ck.Memory)
		}
	}
	if rc.Promote != nil && strings.TrimSpace(rc.Promote.Branch) == "" {
		return nil, fmt.Errorf("promote: branch must be set (the target branch to fast-forward on green)")
	}
	if rc.Scan != nil {
		s := rc.Scan
		if s.Image == "" {
			s.Image = DefaultTrivyImage
		}
		if s.Scanners == "" {
			s.Scanners = DefaultTrivyScanners
		}
		if s.Severity == "" {
			s.Severity = DefaultTrivySeverity
		}
		if s.CacheVolume == "" {
			s.CacheVolume = DefaultTrivyCacheVolume
		}
		if strings.TrimSpace(s.Path) == "" {
			s.Path = "."
		}
		// These values become docker/trivy argv. We pass them as separate args
		// (no shell), but still reject anything outside a conservative charset so
		// a typo can't smuggle an option or path separator surprise.
		for _, f := range []struct{ name, val string }{
			{"scan.image", s.Image}, {"scan.scanners", s.Scanners},
			{"scan.severity", s.Severity}, {"scan.path", s.Path},
			{"scan.cache-volume", s.CacheVolume},
		} {
			if !safeArg(f.val) {
				return nil, fmt.Errorf("%s contains unsupported characters: %q", f.name, f.val)
			}
		}
		if s.Timeout < 0 {
			return nil, fmt.Errorf("scan.timeout must be >= 0")
		}
		// safeArg's charset admits ".", so ".." would pass it — reject any
		// dot-dot element explicitly to keep the scan target inside the repo.
		for _, el := range strings.Split(s.Path, "/") {
			if el == ".." {
				return nil, fmt.Errorf("scan.path must stay inside the repo: %q", s.Path)
			}
		}
	}
	// An explicit cache: block must name at least one absolute path and a prime
	// command. cache:false and auto-detect (Cache nil) need no validation here.
	if rc.Cache != nil && rc.Cache.Explicit {
		if len(rc.Cache.Paths) == 0 {
			return nil, fmt.Errorf("cache.paths must list at least one path")
		}
		if rc.Cache.Prime == "" {
			return nil, fmt.Errorf("cache.prime must be set (the command that populates the cache)")
		}
		for _, p := range rc.Cache.Paths {
			if !safeCachePath(p) {
				return nil, fmt.Errorf("cache.paths entries must be absolute container paths without %q: %q", "..", p)
			}
		}
	}
	return &rc, nil
}

// safeCachePath reports whether p is acceptable as a cache mount target: an
// absolute container path with no ".." element, over the same conservative
// charset as safeArg (so a typo can't smuggle a docker -v option or surprise).
func safeCachePath(p string) bool {
	if !strings.HasPrefix(p, "/") || p == "/" {
		return false
	}
	for _, c := range p {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-' || c == '/':
		default:
			return false
		}
	}
	for _, el := range strings.Split(p, "/") {
		if el == ".." {
			return false
		}
	}
	return true
}

// safeArg reports whether s is safe to pass as a single docker/trivy argument:
// it must start alphanumeric and contain only a conservative set (covers image
// refs like aquasec/trivy:0.58.1@sha256:…, scanner/severity CSV lists, and
// relative paths). Excludes whitespace and shell metacharacters.
func safeArg(s string) bool {
	if s == "" {
		return false
	}
	if !((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z') ||
		(s[0] >= '0' && s[0] <= '9') || s[0] == '.') {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == ',' || c == '.' || c == '_' || c == '-' || c == '/' || c == ':' || c == '@' || c == '+':
		default:
			return false
		}
	}
	return true
}

// parseRef turns "owner/name" + branch into a Ref.
func parseRef(repo, branch string) (Ref, error) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Ref{}, fmt.Errorf("repo %q must be in the form owner/name", repo)
	}
	if branch == "" {
		branch = "main"
	}
	return Ref{Owner: parts[0], Name: parts[1], Branch: branch}, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// secondsOr reads a positive-seconds env var, warning (and falling back to
// def) on anything unparseable rather than silently ignoring it.
func secondsOr(key string, def int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(def) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		log.Printf("config: ignoring invalid %s=%q (want positive seconds); using %ds", key, v, def)
		return time.Duration(def) * time.Second
	}
	return time.Duration(n) * time.Second
}

// intEnv reads a non-negative integer env var, warning (and falling back to
// def) on anything unparseable rather than silently ignoring it.
func intEnv(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		log.Printf("config: ignoring invalid %s=%q (want a non-negative integer); using %d", key, v, def)
		return def
	}
	return n
}

// floatEnv reads a positive-float env var, warning (and falling back to def) on
// anything unparseable rather than silently ignoring it.
func floatEnv(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		log.Printf("config: ignoring invalid %s=%q (want a positive number); using %g", key, v, def)
		return def
	}
	return n
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// boolEnv reads a boolean env var, returning def when unset or unrecognized.
func boolEnv(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
