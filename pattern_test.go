package main

import "testing"

// TestParseRebuild checks that rebuilding from the original segments preserves
// the path structure (leading/trailing slash) and drops any query/fragment.
func TestParseRebuild(t *testing.T) {
	tests := map[string]string{
		"/users/7/":                             "/users/7/",
		"/users/7#profile":                      "/users/7",
		"/orders/42?tab=items":                  "/orders/42",
		"/orders/42?limit=10&name=foo":          "/orders/42",
		"/api/v2/users/3":                       "/api/v2/users/3",
		"/files/order-9f3a8821":                 "/files/order-9f3a8821",
		"/users/john/sessions/a1b2c3d4e5f6a1b2": "/users/john/sessions/a1b2c3d4e5f6a1b2",
		"/":                                     "/",
		"users/7":                               "users/7",
	}
	for in, want := range tests {
		p := parseURL(in)
		if got := p.rebuild(p.segments); got != want {
			t.Errorf("rebuild %q = %q, want %q", in, got, want)
		}
	}
}

// TestHeuristicPattern checks the pure-Go fallback on the obvious-ID cases.
func TestHeuristicPattern(t *testing.T) {
	tests := map[string]string{
		"/users/7/":               "/users/{userId}/",
		"/users/7#profile":        "/users/{userId}",
		"/orders/42?tab=items":    "/orders/{orderId}",
		"/api/v2/users/3":         "/api/v2/users/{userId}",
		"/files/order-9f3a8821":   "/files/{fileId}",
		"/sessions/a1b2c3d4e5f6a": "/sessions/{sessionId}",
		"/builds/9f3a":            "/builds/{buildId}", // short hex w/ digit+letter

		// REST convention: word after a plural collection noun is its id.
		"/users/john": "/users/{userId}",
		"/orgs/acme":  "/orgs/{orgId}",
		"/api/v2/users/john/sessions/a1b2c3d4e5f6a1b2": "/api/v2/users/{userId}/sessions/{sessionId}",

		// Exceptions to the convention stay static.
		"/users/settings": "/users/settings", // sub-resource word
		"/users/me":       "/users/me",       // action word
		"/users/sessions": "/users/sessions", // sub-collection noun
	}
	for in, want := range tests {
		p := parseURL(in)
		got := p.rebuild(applyDynamic(p.segments, heuristicDynamic(p.segments)))
		if got != want {
			t.Errorf("heuristic %q = %q, want %q", in, got, want)
		}
	}
}

// TestApplyDynamic checks deterministic naming from the dynamic flags, including
// the case the model previously got wrong (literal value as placeholder name).
func TestApplyDynamic(t *testing.T) {
	segments := []string{"users", "john", "sessions", "a1b2c3d4e5f6a1b2"}
	dynamic := []bool{false, true, false, true}
	got := applyDynamic(segments, dynamic)
	want := []string{"users", "{userId}", "sessions", "{sessionId}"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("applyDynamic[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestValidate(t *testing.T) {
	in := []string{"users", "7"}

	if err := validate(in, []bool{false, true}); err != nil {
		t.Errorf("valid flags rejected: %v", err)
	}
	if err := validate(in, []bool{false}); err == nil {
		t.Error("wrong length accepted")
	}
}

func TestSingularize(t *testing.T) {
	tests := map[string]string{
		"users":      "user",
		"sessions":   "session",
		"files":      "file",
		"orders":     "order",
		"categories": "category",
		"addresses":  "address",
		"v2":         "v2",
	}
	for in, want := range tests {
		if got := singularize(in); got != want {
			t.Errorf("singularize(%q) = %q, want %q", in, got, want)
		}
	}
}
