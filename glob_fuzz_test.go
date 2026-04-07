package tavern

import (
	"strings"
	"testing"
)

func FuzzMatchGlob(f *testing.F) {
	// Seed corpus: normal patterns and edge cases.
	f.Add("app/dashboard/*", "app/dashboard/metrics")
	f.Add("app/dashboard/**", "app/dashboard/metrics/cpu")
	f.Add("**/alerts", "app/dashboard/alerts")
	f.Add("**", "a/b/c/d")
	f.Add("app/*/status", "app/orders/status")
	f.Add("*", "single")
	f.Add("", "")
	f.Add("", "notempty")
	f.Add("notempty", "")
	f.Add("a/b/c", "a/b/c")
	f.Add("a/b/c", "a/b/d")
	// Edge cases: consecutive separators, deeply nested.
	f.Add("a//b", "a//b")
	f.Add("**/**/**", "a/b/c")
	f.Add("*/*/*/*/*/*", "a/b/c/d/e/f")
	f.Add("**/**/a", "a")
	f.Add("a/**/**", "a")
	// Special characters.
	f.Add("a.b/c+d", "a.b/c+d")
	f.Add("a[b]/c{d}", "a[b]/c{d}")
	f.Add("*", "")
	f.Add("*/", "/")

	f.Fuzz(func(t *testing.T, pattern, topic string) {
		patternSegments := strings.Split(pattern, "/")
		topicSegments := strings.Split(topic, "/")

		// Primary check: no panics.
		result := matchGlob(patternSegments, topicSegments)

		// Invariant: exact match should always succeed.
		if pattern == topic && !strings.Contains(pattern, "*") {
			if !result {
				t.Errorf("exact match failed: pattern=%q topic=%q", pattern, topic)
			}
		}

		// Invariant: "**" should match everything.
		if pattern == "**" && !result {
			t.Errorf("** should match everything, but failed for topic=%q", topic)
		}

		_ = result
	})
}

func FuzzMatchGlobDeepNesting(f *testing.F) {
	// Focus on deeply nested topics to stress the recursive matching.
	f.Add("**", "a/b/c/d/e/f/g/h/i/j")
	f.Add("**/z", "a/b/c/d/e/f/g/h/i/z")
	f.Add("a/**/z", "a/b/c/d/e/f/g/h/i/z")
	f.Add("a/**/b/**/c", "a/x/y/b/z/w/c")
	f.Add("**/**", "a/b/c")

	f.Fuzz(func(t *testing.T, pattern, topic string) {
		// Limit pattern and topic length to avoid excessive recursion on
		// pathological inputs (e.g., many consecutive ** segments).
		if len(pattern) > 200 || len(topic) > 200 {
			return
		}

		patternSegments := strings.Split(pattern, "/")
		topicSegments := strings.Split(topic, "/")

		// Count ** segments; skip if too many to avoid exponential blowup.
		doubleStars := 0
		for _, s := range patternSegments {
			if s == "**" {
				doubleStars++
			}
		}
		if doubleStars > 5 {
			return
		}

		// Primary check: no panics.
		_ = matchGlob(patternSegments, topicSegments)
	})
}
