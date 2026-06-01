// This program extracts a low-cardinality pattern from a URL path using a
// local LLM via Kronk. High-cardinality segments (numeric IDs, UUIDs, hashes,
// usernames, id-bearing slugs) are replaced with named placeholders so the
// patterns can be used as stable labels for metrics, spans, etc.
//
// The structural work (splitting segments, preserving slashes / #fragment /
// ?query) is done deterministically in Go. The LLM only classifies each path
// segment, so a small and fast model is enough. Its output is validated and,
// on failure, the program falls back to a pure-Go heuristic.
//
// The model and llama.cpp libraries are loaded once at startup, then an HTTP
// server exposes an endpoint that extracts patterns from one or several URLs.
//
// The first time you run this program the system will download and install
// the model and libraries.
//
// Run it like this (listens on :8080, override with PORT):
// $ go run .
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
	"github.com/ardanlabs/kronk/sdk/tools/defaults"
	"github.com/ardanlabs/kronk/sdk/tools/libs"
	"github.com/ardanlabs/kronk/sdk/tools/models"
)

// modelSource is the GGUF model to use, overridable with the MODEL env var.
// A small, fast model is enough because Go handles all structural work,
// placeholder naming, and strong ID detection; the LLM only adds the
// human-readable-identifier judgment (usernames, org slugs). 0.6B handles the
// structure but misses those, so 1.7B is the cheap/fast sweet spot.
var modelSource = envStr("MODEL", "unsloth/Qwen3-1.7B-Q8_0")

func main() {
	// "warmup" downloads the libraries and model, then exits. It is run during
	// the Docker image build so the running container starts ready to serve.
	if len(os.Args) > 1 && os.Args[1] == "warmup" {
		if _, err := installSystem(); err != nil {
			fmt.Printf("\nERROR: warmup: %s\n", err)
			os.Exit(1)
		}
		fmt.Printf("warmup complete: libraries and model %q are cached\n", modelSource)
		return
	}

	if err := run(); err != nil {
		fmt.Printf("\nERROR: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Load llama.cpp and the model once; the loaded model is shared by the
	// HTTP server for the lifetime of the process.
	mp, err := installSystem()
	if err != nil {
		return fmt.Errorf("unable to install system: %w", err)
	}

	nSeqMax := envInt("NSEQ", 4)

	krn, err := newKronk(mp, nSeqMax)
	if err != nil {
		return fmt.Errorf("unable to init kronk: %w", err)
	}

	defer func() {
		fmt.Println("unloading model...")
		if err := krn.Unload(context.Background()); err != nil {
			fmt.Printf("failed to unload model: %v", err)
		}
	}()

	addr := ":8080"
	if p := os.Getenv("PORT"); p != "" {
		addr = ":" + p
	}

	return serve(krn, addr, nSeqMax)
}

// envInt reads a positive integer from an environment variable, falling back to
// def when unset or invalid.
func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// envStr reads a string environment variable, falling back to def when unset.
func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func installSystem() (models.Path, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	libs, err := libs.New(
		libs.WithVersion(defaults.LibVersion("")),
	)
	if err != nil {
		return models.Path{}, err
	}

	if _, err := libs.Download(ctx, kronk.FmtLogger); err != nil {
		return models.Path{}, fmt.Errorf("unable to install llama.cpp: %w", err)
	}

	// -------------------------------------------------------------------------

	mdls, err := models.New()
	if err != nil {
		return models.Path{}, fmt.Errorf("unable to init models: %w", err)
	}

	mp, err := mdls.Download(ctx, kronk.FmtLogger, modelSource)
	if err != nil {
		return models.Path{}, fmt.Errorf("unable to install model: %w", err)
	}

	return mp, nil
}

func newKronk(mp models.Path, nSeqMax int) (*kronk.Kronk, error) {
	fmt.Printf("loading model (nSeqMax=%d)...\n", nSeqMax)

	if err := kronk.Init(); err != nil {
		return nil, fmt.Errorf("unable to init kronk: %w", err)
	}

	// NSeqMax sets how many sequences the single model instance can process in
	// parallel. The context window is shared across sequences, so size it to
	// give each sequence room for our prompt (~1.5k tokens) plus output.
	krn, err := kronk.New(
		model.WithModelFiles(mp.ModelFiles),
		model.WithNSeqMax(nSeqMax),
		model.WithContextWindow(nSeqMax*2048),
		model.WithLog(kronk.FmtLogger),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to create inference model: %w", err)
	}

	return krn, nil
}
