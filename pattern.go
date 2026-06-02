package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
)

// maxAttempts is how many times we ask the model for one segment before giving
// up and falling back to the Go heuristic value for that segment.
const maxAttempts = 3

// Extractor turns URLs into low-cardinality patterns. It holds the shared,
// single loaded model and a gate that bounds the number of concurrent model
// calls to NSeqMax (the model's parallel-sequence capacity). Segment
// classification within a URL is fanned out concurrently through that gate.
type Extractor struct {
	krn  *kronk.Kronk
	gate chan struct{}
}

// newExtractor creates an Extractor whose concurrent model calls are capped at
// nSeqMax, matching the model's parallel sequence capacity.
func newExtractor(krn *kronk.Kronk, nSeqMax int) *Extractor {
	return &Extractor{
		krn:  krn,
		gate: make(chan struct{}, nSeqMax),
	}
}

// Extract turns a concrete URL into a low-cardinality pattern. All structural
// work AND placeholder naming are deterministic; the model only answers, per
// segment, whether it is a dynamic identifier. Each segment the heuristic can't
// already resolve is classified concurrently (bounded by the gate), and the
// model can only promote a segment to dynamic — never demote one the heuristic
// is already sure about.
func (e *Extractor) Extract(ctx context.Context, url string) (string, error) {
	p := parseURL(url)

	debugf("--- resolution ---")
	debugf("model     : %s", modelSource)
	debugf("url       : %s", url)
	debugf("parse     : segments=%v leading=%t trailing=%t stripped=%q", p.segments, p.leading, p.trailing, p.suffix)

	// Nothing to classify (e.g. "/" or ""): return the input unchanged.
	if len(p.segments) == 0 {
		debugf("classify  : no segments, returning input unchanged")
		return url, nil
	}

	// The Go heuristic confidently catches numeric/UUID/hash/slug IDs and the
	// REST collection-id convention; those segments are final and need no model
	// call. Only the remaining (heuristic-static) segments are sent to the
	// model, each in its own goroutine, to catch human-readable identifiers the
	// heuristic misses (e.g. an id after a singular resource).
	guesses := heuristicDynamic(p.segments)
	debugf("heuristic : %s", fmtFlags(p.segments, guesses))

	dynamic := make([]bool, len(p.segments))
	copy(dynamic, guesses)
	notes := make([]string, len(p.segments))

	var wg sync.WaitGroup
	for i := range p.segments {
		if guesses[i] {
			notes[i] = "heuristic"
			continue
		}
		// The first segment is the route root (api, users, ...) — it is never a
		// bare identifier in practice, so don't spend a model call on it (and
		// avoid the model wrongly flagging it dynamic). A leading numeric/UUID is
		// still caught by the heuristic above.
		if i == 0 {
			notes[i] = "root-static"
			continue
		}
		if confidentlyStatic(p.segments[i]) {
			notes[i] = "static-rule"
			continue
		}

		wg.Add(1)
		go func() {
			defer wg.Done()

			prev := ""
			if i > 0 {
				prev = p.segments[i-1]
			}

			switch got, err := e.classifySegment(ctx, prev, p.segments[i]); {
			case err != nil:
				notes[i] = "model-err" // keep heuristic (static)
			case got:
				dynamic[i] = true
				notes[i] = "model-DYN"
			default:
				notes[i] = "model-static"
			}
		}()
	}
	wg.Wait()

	queried := 0
	for _, n := range notes {
		if strings.HasPrefix(n, "model") {
			queried++
		}
	}

	debugf("classify  : %s", fmtNotes(p.segments, notes))
	debugf("llm calls : %d segment(s) sent to the model (of %d)", queried, len(p.segments))
	debugf("combined  : %s", fmtFlags(p.segments, dynamic))

	tokens := applyDynamic(p.segments, dynamic)
	debugf("tokens    : %v", tokens)

	pattern := p.rebuild(tokens)
	debugf("pattern   : %s", pattern)
	debugf("------------------")

	return pattern, nil
}

// debug controls the step-by-step resolution trace. On by default; disable with
// URL_DETECT_DEBUG=0.
var debug = os.Getenv("URL_DETECT_DEBUG") != "0"

func debugf(format string, args ...any) {
	if debug {
		fmt.Printf(format+"\n", args...)
	}
}

// fmtFlags renders each segment with its dynamic flag, e.g. "users=static 7=DYN".
func fmtFlags(segments []string, flags []bool) string {
	parts := make([]string, len(segments))
	for i, seg := range segments {
		state := "static"
		if i < len(flags) && flags[i] {
			state = "DYN"
		}
		parts[i] = fmt.Sprintf("%s=%s", seg, state)
	}
	return strings.Join(parts, " ")
}

// fmtNotes renders each segment with how it was decided, e.g.
// "users=heuristic 7=heuristic john=model-DYN".
func fmtNotes(segments []string, notes []string) string {
	parts := make([]string, len(segments))
	for i, seg := range segments {
		parts[i] = fmt.Sprintf("%s=%s", seg, notes[i])
	}
	return strings.Join(parts, " ")
}

// -----------------------------------------------------------------------------
// Structural parsing / reconstruction (deterministic, never delegated).

type parsedURL struct {
	leading  bool     // path starts with "/"
	trailing bool     // path ends with "/" (and is more than just "/")
	segments []string // non-empty path segments, in order
	suffix   string   // "?query" and/or "#fragment", split off and dropped
}

func parseURL(url string) parsedURL {
	// Split off the first #fragment or ?query; keep it byte-for-byte.
	path := url
	var suffix string
	if i := strings.IndexAny(url, "?#"); i >= 0 {
		path, suffix = url[:i], url[i:]
	}

	p := parsedURL{
		leading:  strings.HasPrefix(path, "/"),
		trailing: len(path) > 1 && strings.HasSuffix(path, "/"),
		suffix:   suffix,
	}

	if core := strings.Trim(path, "/"); core != "" {
		p.segments = strings.Split(core, "/")
	}

	return p
}

// rebuild reconstructs the pattern from the (classified) segments. The query
// string and fragment captured in p.suffix are intentionally dropped: they are
// high-cardinality and not part of the route pattern.
func (p parsedURL) rebuild(segments []string) string {
	var b strings.Builder
	if p.leading {
		b.WriteByte('/')
	}
	b.WriteString(strings.Join(segments, "/"))
	if p.trailing {
		b.WriteByte('/')
	}
	return b.String()
}

// -----------------------------------------------------------------------------
// Heuristic classification (Go). Catches the obvious identifiers and proposes
// placeholder names; also serves as the fallback when the model misbehaves.

var (
	allDigitsRE = regexp.MustCompile(`^[0-9]+$`)
	uuidRE      = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexTokenRE  = regexp.MustCompile(`^[0-9a-fA-F]{8,}$`)
	shortHexRE  = regexp.MustCompile(`^[0-9a-fA-F]{3,}$`)
	hasDigitRE  = regexp.MustCompile(`[0-9]`)
	hasHexAlpha = regexp.MustCompile(`[a-fA-F]`)
)

// looksDynamic reports whether a segment is obviously a high-cardinality
// identifier. Human-readable identifiers (usernames, org slugs) are NOT caught
// here on purpose; that is what the model is for.
func looksDynamic(seg string) bool {
	switch {
	case allDigitsRE.MatchString(seg):
		return true
	case uuidRE.MatchString(seg):
		return true
	case hexTokenRE.MatchString(seg):
		return true
	case shortHexRE.MatchString(seg) && hasDigitRE.MatchString(seg) && hasHexAlpha.MatchString(seg):
		// short hash-like token mixing digits and hex letters, e.g. "9f3a".
		// Pure-letter words ("cafe", "beef") and bare numbers are excluded.
		return true
	case hasDigitRE.MatchString(seg) && strings.ContainsAny(seg, "-_"):
		// id-bearing slug, e.g. "order-9f3a8821".
		return true
	default:
		return false
	}
}

// collectionNounRE matches a plural resource name: lowercase letters ending in
// "s" (users, sessions, orders, files, orgs, builds, projects, ...).
var collectionNounRE = regexp.MustCompile(`^[a-z]{3,}s$`)

// subResourceWords are words that commonly follow a collection but are NOT an
// identifier (a sub-resource or an action), e.g. /users/me, /users/search.
// They keep the REST-convention rule from flagging them as dynamic.
var subResourceWords = map[string]bool{
	"me": true, "self": true, "current": true, "all": true, "any": true,
	"new": true, "edit": true, "create": true, "search": true, "count": true,
	"summary": true, "stats": true, "export": true, "import": true,
	"recent": true, "latest": true, "popular": true, "default": true,
	"login": true, "logout": true, "signup": true, "password": true,
	"profile": true, "avatar": true, "status": true, "health": true,
	"config": true, "settings": true,
}

func isCollectionNoun(seg string) bool {
	return collectionNounRE.MatchString(seg)
}

// versionRE matches an API version token such as "v1" or "v2".
var versionRE = regexp.MustCompile(`^v[0-9]+$`)

// confidentlyStatic reports whether a (heuristic-static) segment is so clearly a
// fixed route name that it needs no model call: a plural collection noun
// (users, sessions), a version token (v2), or a known sub-resource/action word
// (settings, me, search). Skipping these is the main speed-up — most segments
// resolve with zero model calls, leaving only genuinely ambiguous ones (e.g. an
// identifier after a singular resource like /user/john) for the model.
func confidentlyStatic(seg string) bool {
	return isCollectionNoun(seg) || versionRE.MatchString(seg) || subResourceWords[seg]
}

// afterCollection applies the REST convention: the segment immediately after a
// plural collection noun is that collection's identifier (users/john -> john),
// unless it is itself a collection noun (a sub-collection) or a known
// sub-resource/action word. This deterministically catches human-readable IDs
// the token-shape heuristic cannot recognize.
func afterCollection(segments []string, i int) bool {
	if i == 0 {
		return false
	}
	seg := segments[i]
	return isCollectionNoun(segments[i-1]) &&
		!isCollectionNoun(seg) &&
		!subResourceWords[seg]
}

// heuristicDynamic returns, per segment, whether it looks like a dynamic
// identifier. Used as a hint to the model and as the fallback classification.
func heuristicDynamic(segments []string) []bool {
	flags := make([]bool, len(segments))
	for i, seg := range segments {
		flags[i] = looksDynamic(seg) || afterCollection(segments, i)
	}
	return flags
}

// applyDynamic builds the output token for each segment from the dynamic flags:
// the original text for static segments, or a deterministically-named
// "{name}" placeholder for dynamic ones. Naming uses the nearest preceding
// static segment, so the model can never influence the placeholder name.
func applyDynamic(segments []string, dynamic []bool) []string {
	out := make([]string, len(segments))
	var prevStatic string
	used := make(map[string]int)
	for i, seg := range segments {
		if dynamic[i] {
			name := placeholderName(prevStatic)
			// Consecutive dynamic segments share the same preceding static
			// segment; suffix a counter so names stay unique (id, id2, ...).
			used[name]++
			if n := used[name]; n > 1 {
				name = fmt.Sprintf("%s%d", name, n)
			}
			out[i] = "{" + name + "}"
			continue
		}
		out[i] = seg
		prevStatic = seg
	}
	return out
}

// placeholderName derives a name from the preceding static segment, e.g.
// "users" -> "userId", "sessions" -> "sessionId". Falls back to "id".
func placeholderName(prevStatic string) string {
	if prevStatic == "" {
		return "id"
	}
	return singularize(prevStatic) + "Id"
}

func singularize(s string) string {
	switch {
	case strings.HasSuffix(s, "ies") && len(s) > 3:
		return s[:len(s)-3] + "y"
	case strings.HasSuffix(s, "ses") || strings.HasSuffix(s, "xes") ||
		strings.HasSuffix(s, "zes") || strings.HasSuffix(s, "ches") ||
		strings.HasSuffix(s, "shes"):
		return s[:len(s)-2]
	case strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss"):
		return s[:len(s)-1]
	default:
		return s
	}
}

// -----------------------------------------------------------------------------
// Model classification. The model never sees slashes; it only decides, for a
// single segment (given its predecessor as context), whether it is dynamic.

// segmentInput is the per-segment classification request sent to the model.
type segmentInput struct {
	Prev    string `json:"prev"`
	Segment string `json:"segment"`
}

// classifySegment asks the model whether a single path segment is a dynamic
// identifier, given the preceding segment as context. The call is bounded by
// the Extractor gate so concurrent model calls never exceed NSeqMax.
func (e *Extractor) classifySegment(ctx context.Context, prev, seg string) (bool, error) {
	// Acquire a model-call slot (or bail if the request is cancelled).
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		return false, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	in, err := json.Marshal(segmentInput{Prev: prev, Segment: seg})
	if err != nil {
		return false, fmt.Errorf("marshal input: %w", err)
	}

	schema := model.D{
		"type": "object",
		"properties": model.D{
			"dynamic": model.D{"type": "boolean"},
		},
		"required": []string{"dynamic"},
	}

	d := model.D{
		"messages": model.DocumentArray(
			model.TextMessage(model.RoleSystem, segmentSystemPrompt),
			model.TextMessage(model.RoleUser, string(in)),
		),
		"enable_thinking": false,
		"json_schema":     schema,
		"temperature":     0.0,
		"max_tokens":      16,
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		resp, err := e.krn.Chat(ctx, d)
		if err != nil {
			lastErr = fmt.Errorf("chat: %w", err)
			debugf("  llm[%-12s] prev=%-10q attempt=%d ERROR: %v", seg, prev, attempt, err)
			continue
		}

		content := strings.TrimSpace(resp.Choices[0].Message.Content)
		debugf("  llm[%-12s] prev=%-10q attempt=%d resp=%s", seg, prev, attempt, content)

		var parsed struct {
			Dynamic bool `json:"dynamic"`
		}
		if err := json.Unmarshal([]byte(content), &parsed); err != nil {
			lastErr = fmt.Errorf("unmarshal model output %q: %w", content, err)
			continue
		}

		return parsed.Dynamic, nil
	}

	return false, lastErr
}

const segmentSystemPrompt = `You classify ONE URL path segment to reduce cardinality for metrics and traces.

Input JSON: {"prev":"<the previous path segment, or empty>","segment":"<the segment to classify>"}

Return {"dynamic":true} if SEGMENT is a value unique to one entity:
a number, a UUID, a hash, a random hex/alphanumeric token (e.g. "9f3a", "a1b2c3d4"),
a username, an account/org/team/project slug, or any id-bearing slug ("order-9f3a8821").

Return {"dynamic":false} if SEGMENT is a fixed name shared by many requests:
an API version or resource/collection/route name (api, v2, users, orders, files,
sessions, projects, builds), or an action/sub-resource word (search, settings, me,
new, export).

Use "prev" as context: a segment that directly follows a resource/collection name
is usually that resource's IDENTIFIER, even when it reads like an ordinary word.

EXAMPLES:
{"prev":"","segment":"api"} -> {"dynamic":false}
{"prev":"api","segment":"v2"} -> {"dynamic":false}
{"prev":"v2","segment":"users"} -> {"dynamic":false}
{"prev":"users","segment":"john"} -> {"dynamic":true}
{"prev":"users","segment":"settings"} -> {"dynamic":false}
{"prev":"user","segment":"john"} -> {"dynamic":true}
{"prev":"orders","segment":"42"} -> {"dynamic":true}
{"prev":"projects","segment":"acme-prod"} -> {"dynamic":true}

Return ONLY the JSON document {"dynamic":true} or {"dynamic":false}.`
