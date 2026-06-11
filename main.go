// Command poll-ci is a dead-simple, single-container CI for GitHub. It watches
// one or more repositories by polling (outbound only — no webhooks, no inbound
// ports), runs each repo's .poll-ci.yml checks in Docker, and reports results
// as native GitHub commit statuses. One process, one container, no database.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	log.SetFlags(log.LstdFlags)
	log.SetOutput(os.Stderr)

	var (
		once    = flag.String("once", "", "process a single commit SHA for the configured repo and exit")
		showVer = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("poll-ci", version)
		return
	}

	cfg, err := LoadRunnerConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	gh := NewGitHub(cfg.Token, cfg.APIBase)
	store, err := LoadStore(cfg.StateFile)
	if err != nil {
		log.Fatalf("state: %v", err)
	}
	runner := NewRunner(cfg, gh, store)

	// Cancel cleanly on Ctrl-C / SIGTERM (docker stop) so in-flight checks unwind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Ad-hoc one-shot mode: run a single commit and exit non-zero if it failed.
	if *once != "" {
		if len(cfg.Refs) != 1 {
			log.Fatalf("--once needs exactly one repo (set REPO=owner/name, not REPOS_FILE)")
		}
		if runner.RunOnce(ctx, cfg.Refs[0], *once) {
			return
		}
		os.Exit(1)
	}

	log.Printf("poll-ci %s starting; interval %s; work dir %s", version, cfg.PollInterval, cfg.WorkDir)
	for _, ref := range cfg.Refs {
		log.Printf("watching %s", ref)
	}
	if cfg.PollPRs {
		log.Printf("also polling open same-repo PR heads")
	}

	// Clean up after any previous process that died mid-run: stale containers
	// and checkouts first, then statuses it left pending on GitHub.
	runner.sweepOrphans(ctx)
	runner.reconcileInFlight(ctx)

	// Poll loop: sweep all refs, then wait POLL_INTERVAL (or exit on signal).
	for {
		runner.PollOnce(ctx)
		select {
		case <-ctx.Done():
			log.Printf("shutting down")
			return
		case <-time.After(cfg.PollInterval):
		}
	}
}
