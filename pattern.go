package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/ardanlabs/kronk/sdk/kronk"
	"github.com/ardanlabs/kronk/sdk/kronk/model"
)

// maxAttempts is how many times we ask the model before falling back to the
// pure-Go heuristic classification.
const maxAttempts = 3

// ExtractPattern turns a concrete URL into a low-cardinality pattern. All
// structural work AND placeholder naming are deterministic; the model only
// answers, per segment, whether it is a dynamic identifier. That answer is
// validated, with a fallback to a Go heuristic.
func ExtractPattern(ctx context.Context, krn *kronk.Kronk, url string) (string, error) {
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

	// The Go heuristic confidently catches numeric/UUID/hash/slug IDs. The
	// model's job is only to ADD the human-readable identifiers the heuristic
	// misses (usernames, org slugs). We therefore OR the two: the model can
	// promote a segment to dynamic but can never demote one the heuristic is
	// sure about, which prevents a weak model from dropping an obvious ID.
	guesses := heuristicDynamic(p.segments)
	debugf("heuristic : %s", fmtFlags(p.segments, guesses))

	dynamic := guesses
	source := "heuristic only (model fallback)"
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		got, err := classifySegments(ctx, krn, p.segments, guesses)
		if err != nil {
			debugf("attempt %d : model call failed: %v", attempt, err)
			continue
		}
		if err := validate(p.segments, got); err != nil {
			debugf("attempt %d : rejected model output: %v", attempt, err)
			continue
		}
		debugf("model     : %s (attempt %d)", fmtFlags(p.segments, got), attempt)
		dynamic = orFlags(guesses, got)
		source = "heuristic OR model"
		break
	}

	debugf("combined  : %s [%s]", fmtFlags(p.segments, dynamic), source)

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

// orFlags combines the heuristic and model classifications: a segment is
// dynamic if either source says so.
func orFlags(a, b []bool) []bool {
	out := make([]bool, len(a))
	for i := range a {
		out[i] = a[i] || b[i]
	}
	return out
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
// Guardrail: validate the model output is structurally consistent with input.

func validate(segments []string, dynamic []bool) error {
	if len(dynamic) != len(segments) {
		return fmt.Errorf("got %d flags, want %d", len(dynamic), len(segments))
	}
	return nil
}

// -----------------------------------------------------------------------------
// Model classification. The model never sees slashes; it only decides, per
// segment, whether to keep it or replace it with a placeholder.

type segmentHint struct {
	Index   int    `json:"i"`
	Segment string `json:"segment"`
	Guess   bool   `json:"guessDynamic"`
}

func classifySegments(ctx context.Context, krn *kronk.Kronk, segments []string, guesses []bool) ([]bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	hints := make([]segmentHint, len(segments))
	for i := range segments {
		hints[i] = segmentHint{Index: i, Segment: segments[i], Guess: guesses[i]}
	}
	hintsJSON, err := json.Marshal(hints)
	if err != nil {
		return nil, fmt.Errorf("marshal hints: %w", err)
	}

	schema := model.D{
		"type": "object",
		"properties": model.D{
			"dynamic": model.D{
				"type":  "array",
				"items": model.D{"type": "boolean"},
			},
		},
		"required": []string{"dynamic"},
	}

	d := model.D{
		"messages": model.DocumentArray(
			model.TextMessage(model.RoleSystem, systemPrompt),
			model.TextMessage(model.RoleUser, string(hintsJSON)),
		),
		"enable_thinking": false,
		"json_schema":     schema,
		"temperature":     0.0,
		"max_tokens":      256,
	}

	resp, err := krn.Chat(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("chat: %w", err)
	}

	content := strings.TrimSpace(resp.Choices[0].Message.Content)

	var parsed struct {
		Dynamic []bool `json:"dynamic"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("unmarshal model output %q: %w", content, err)
	}

	return parsed.Dynamic, nil
}

const systemPrompt = `You classify URL PATH SEGMENTS to reduce cardinality for metrics and traces.

You are given a JSON array of path segments in order. Each item has:
- "segment": the literal text of that path segment
- "guessDynamic": a heuristic guess of whether the segment is dynamic

For EACH item, return a boolean: true if the segment is DYNAMIC, false if it is STATIC.
- STATIC: a fixed resource or route name shared by many requests (api, v2, users, files, sessions, orders, posts, builds, ...).
- DYNAMIC: a per-entity identifier unique to one entity: a number, a UUID, a hash, a long or short random hex/alphanumeric token (e.g. "9f3a", "a1b2c3d4"), a USERNAME, an org/team/project slug, or an id-bearing slug like "order-9f3a8821".

The "guessDynamic" hint is reliable for numeric and long-hash IDs, but it MISSES human-readable identifiers such as usernames ("john") and slugs ("acme") and short tokens ("9f3a") — mark those true even when the guess is false.

KEY RULE: the segment immediately AFTER a collection noun (a plural resource name like users, sessions, orders, projects, files, teams, repos) is that collection's IDENTIFIER. Mark it DYNAMIC even when it reads like an ordinary word (users/john -> john is dynamic; orgs/acme -> acme is dynamic). This applies ANYWHERE in the path, not only near the start (api/v2/users/john -> john is dynamic). The only exception is when that following segment is itself a sub-resource/collection noun or an action word (e.g. users/settings, users/search, users/me) — those stay STATIC.

The FIRST segment is almost always a static collection name (api, users, orgs, ...). Do not mark it dynamic unless it is clearly an identifier.

EXAMPLES:
Input: [{"i":0,"segment":"api","guessDynamic":false},{"i":1,"segment":"v2","guessDynamic":false},{"i":2,"segment":"users","guessDynamic":false},{"i":3,"segment":"42","guessDynamic":true}]
Output: {"dynamic":[false,false,false,true]}

Input: [{"i":0,"segment":"users","guessDynamic":false},{"i":1,"segment":"john","guessDynamic":false},{"i":2,"segment":"sessions","guessDynamic":false},{"i":3,"segment":"9f3a2b1c","guessDynamic":true}]
Output: {"dynamic":[false,true,false,true]}

Input: [{"i":0,"segment":"api","guessDynamic":false},{"i":1,"segment":"v2","guessDynamic":false},{"i":2,"segment":"users","guessDynamic":false},{"i":3,"segment":"john","guessDynamic":false},{"i":4,"segment":"sessions","guessDynamic":false},{"i":5,"segment":"a1b2c3d4e5f6a1b2","guessDynamic":true}]
Output: {"dynamic":[false,false,false,true,false,true]}

Input: [{"i":0,"segment":"orgs","guessDynamic":false},{"i":1,"segment":"acme","guessDynamic":false},{"i":2,"segment":"projects","guessDynamic":false},{"i":3,"segment":"12","guessDynamic":true},{"i":4,"segment":"builds","guessDynamic":false},{"i":5,"segment":"9f3a","guessDynamic":true}]
Output: {"dynamic":[false,true,false,true,false,true]}

Input: [{"i":0,"segment":"users","guessDynamic":false},{"i":1,"segment":"settings","guessDynamic":false}]
Output: {"dynamic":[false,false]}

Return JSON: {"dynamic":[ ... ]} with EXACTLY one boolean per input item, in the SAME order. Do not add, remove, or reorder items. Return ONLY the JSON document.`
