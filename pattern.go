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

// Warm primes the model's incremental cache (IMC) so the shared system prompt
// is already cached in every sequence before real traffic arrives. The first
// call on a cold sequence pays a one-time prefill of the whole system prompt
// (seconds on CPU); by firing one dummy classification per sequence here, that
// cost is paid once at startup instead of on the first user requests. Errors
// are ignored — warming is best-effort.
func (e *Extractor) Warm(ctx context.Context) {
	n := cap(e.gate)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = e.classifySegment(ctx, "warmup", "warmup", false)
		}()
	}
	wg.Wait()
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

	// prior[i] = "looks like an identifier": a confident token SHAPE (number,
	// UUID, hash, id-slug) OR the REST collection-follow convention. Shapes are
	// final. The collection-follow signal is only a *prior*: the segment after a
	// collection is usually its id, but can be a fixed sub-resource/keyword
	// (assets/img, classes/Foo, payments/stripe), so it is sent to the model to
	// confirm or demote rather than decided here.
	prior := heuristicDynamic(p.segments)
	debugf("heuristic : %s", fmtFlags(p.segments, prior))

	dynamic := make([]bool, len(p.segments))
	notes := make([]string, len(p.segments))

	var wg sync.WaitGroup
	for i := range p.segments {
		seg := p.segments[i]

		// Confident token shape -> dynamic, no model call.
		if looksDynamic(seg) {
			dynamic[i] = true
			notes[i] = "shape"
			continue
		}
		// The first segment is the route root (api, users, ...) — never a bare
		// identifier in practice.
		if i == 0 {
			notes[i] = "root-static"
			continue
		}
		// Plural collection / version / known sub-resource word -> static.
		if confidentlyStatic(seg) {
			notes[i] = "static-rule"
			continue
		}
		// REST alternates collection/id/sub-resource/id/... so a word-like
		// segment following an identifier is a sub-resource NAME, not another id.
		if prior[i-1] {
			notes[i] = "after-id-static"
			continue
		}
		// Forward alternation: a word that follows a collection AND is immediately
		// followed by an identifier is a sub-resource NAME — the id is the next
		// segment, not this one (e.g. videos/metadata/1 -> metadata static, 1 id).
		if i+1 < len(p.segments) && afterCollection(p.segments, i) && looksDynamic(p.segments[i+1]) {
			notes[i] = "before-id-static"
			continue
		}

		// Ambiguous word — ask the model. coll tells it whether this segment
		// follows a collection (prior leans dynamic) or a plain word (prior leans
		// static), so it only overrides the obvious cases.
		coll := afterCollection(p.segments, i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := e.classifySegment(ctx, p.segments[i-1], seg, coll)
			switch {
			case err != nil:
				dynamic[i] = coll // fall back to the prior
				notes[i] = "model-err"
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
	Prev              string `json:"prev"`
	Segment           string `json:"segment"`
	FollowsCollection bool   `json:"followsCollection"`
}

// classifySegment asks the model whether a single path segment is a dynamic
// identifier, given the preceding segment as context and whether it follows a
// collection (the prior). The call is bounded by the Extractor gate so
// concurrent model calls never exceed NSeqMax.
func (e *Extractor) classifySegment(ctx context.Context, prev, seg string, followsCollection bool) (bool, error) {
	// Acquire a model-call slot (or bail if the request is cancelled).
	select {
	case e.gate <- struct{}{}:
		defer func() { <-e.gate }()
	case <-ctx.Done():
		return false, ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	in, err := json.Marshal(segmentInput{Prev: prev, Segment: seg, FollowsCollection: followsCollection})
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
			debugf("  llm[%-12s] prev=%-10q coll=%-5t attempt=%d ERROR: %v", seg, prev, followsCollection, attempt, err)
			continue
		}

		// Some model/llama.cpp combinations return a response with no choices (or
		// a nil message) instead of an error. Treat that as a failed attempt so we
		// retry and ultimately fall back to the heuristic, rather than panicking.
		if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
			lastErr = fmt.Errorf("model returned no message")
			debugf("  llm[%-12s] prev=%-10q coll=%-5t attempt=%d EMPTY (choices=%d)", seg, prev, followsCollection, attempt, len(resp.Choices))
			continue
		}

		content := strings.TrimSpace(resp.Choices[0].Message.Content)
		debugf("  llm[%-12s] prev=%-10q coll=%-5t attempt=%d resp=%s", seg, prev, followsCollection, attempt, content)

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

const segmentSystemPrompt = `You classify ONE URL path segment as dynamic (a per-entity value) or static (a fixed route/keyword), to reduce cardinality for metrics and traces.

Input JSON: {"prev":"<previous segment>","segment":"<segment to classify>","followsCollection":<bool>}

dynamic=true  — a value unique to one entity: a number, a UUID, a hash, a random token,
a username, an account/org/team/project/region/city name, or an id-bearing slug.
dynamic=false — a fixed name shared by many requests: a resource/collection/route name,
an action/sub-resource (search, settings, me, new), a version (v2, v10),
a file or directory (img, css, main.css), a protocol/format/algorithm (http2, sha256, json),
a known provider/brand (stripe, github, aws), or a CamelCase type name.

How to use "followsCollection":
- true: SEGMENT follows a collection, so it is USUALLY that collection's identifier.
  Answer dynamic=true UNLESS it is clearly one of the fixed keywords above
  (file/dir, protocol/format, provider, version, CamelCase type, or a reserved word
  like active/latest/me).
- false: SEGMENT follows a non-collection word, so it is USUALLY a fixed sub-route.
  Answer dynamic=false UNLESS it is clearly a value (a number, a hash, a username, a slug).

EXAMPLES:
{"prev":"users","segment":"john","followsCollection":true} -> {"dynamic":true}
{"prev":"orgs","segment":"acme","followsCollection":true} -> {"dynamic":true}
{"prev":"regions","segment":"us","followsCollection":true} -> {"dynamic":true}
{"prev":"assets","segment":"img","followsCollection":true} -> {"dynamic":false}
{"prev":"protocols","segment":"http2","followsCollection":true} -> {"dynamic":false}
{"prev":"payments","segment":"stripe","followsCollection":true} -> {"dynamic":false}
{"prev":"classes","segment":"SNSChatParticipant","followsCollection":true} -> {"dynamic":false}
{"prev":"projects","segment":"active","followsCollection":true} -> {"dynamic":false}
{"prev":"user","segment":"jane","followsCollection":false} -> {"dynamic":true}
{"prev":"shop","segment":"checkout","followsCollection":false} -> {"dynamic":false}
{"prev":"boutique","segment":"panier","followsCollection":false} -> {"dynamic":false}
{"prev":"docs","segment":"api","followsCollection":false} -> {"dynamic":false}

Return ONLY the JSON document {"dynamic":true} or {"dynamic":false}.`
