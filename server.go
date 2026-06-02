package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk"
)

// server exposes the Extractor over HTTP. The Extractor owns the single loaded
// model and bounds concurrent model calls (across requests, URLs, and the
// per-segment fan-out) to NSeqMax.
type server struct {
	ex *Extractor
}

type patternRequest struct {
	URL  string   `json:"url,omitempty"`  // a single URL
	URLs []string `json:"urls,omitempty"` // or a batch of URLs
}

type patternResult struct {
	URL     string `json:"url"`
	Pattern string `json:"pattern,omitempty"`
	Error   string `json:"error,omitempty"`
}

type patternResponse struct {
	Results []patternResult `json:"results"`
}

// serve loads the routes and runs the HTTP server until an interrupt signal,
// then shuts down gracefully so the deferred model unload can run.
func serve(krn *kronk.Kronk, addr string, nSeqMax int) error {
	srv := &server{
		ex: newExtractor(krn, nSeqMax),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /patterns", srv.handlePatterns)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	httpServer := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Bind before announcing readiness so the log line is only printed once the
	// port is actually held (and a bind failure surfaces immediately).
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("listening on %s — POST /patterns (max %d concurrent)\n", addr, nSeqMax)
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		fmt.Println("\nshutting down...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// handlePatterns accepts a JSON body of {"url": "..."} and/or
// {"urls": ["...", ...]} and returns the extracted pattern for each URL.
func (s *server) handlePatterns(w http.ResponseWriter, r *http.Request) {
	var req patternRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON body: %v", err), http.StatusBadRequest)
		return
	}

	urls := req.URLs
	if req.URL != "" {
		urls = append(urls, req.URL)
	}
	if len(urls) == 0 {
		http.Error(w, `provide "url" or "urls" in the JSON body`, http.StatusBadRequest)
		return
	}

	// Process the URLs in a batch concurrently; the gate inside the Extractor
	// caps the actual model parallelism at NSeqMax. Results keep their order.
	resp := patternResponse{Results: make([]patternResult, len(urls))}
	var wg sync.WaitGroup
	wg.Add(len(urls))
	for i, url := range urls {
		go func() {
			defer wg.Done()
			resp.Results[i] = s.extract(r.Context(), url)
		}()
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		fmt.Printf("encode response: %v\n", err)
	}
}

// extract runs one extraction. Concurrency is bounded inside the Extractor (its
// gate), so each URL just gets a request-scoped timeout here. Returns the
// pattern or an error message for the URL.
func (s *server) extract(ctx context.Context, url string) patternResult {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	pattern, err := s.ex.Extract(ctx, url)
	if err != nil {
		return patternResult{URL: url, Error: err.Error()}
	}
	return patternResult{URL: url, Pattern: pattern}
}
