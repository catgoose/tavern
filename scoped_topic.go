package tavern

import "strings"

// ScopedTopic builds an app-level scoped topic name from a base and an
// ordered sequence of parts, joined with a colon: base:part:part:...
//
// It is a naming-only convention for flat, colon-delimited topic strings.
// It is distinct from topic-path scoping (slash-separated, glob-matched) and
// from broker scoping ([SSEBroker.SubscribeScoped] / [SSEBroker.PublishTo]);
// see docs/topic-semantics.md for choosing between them.
//
//	ScopedTopic("sales-goals")                                   // "sales-goals"
//	ScopedTopic("sales-goals", "subdivision", "12")              // "sales-goals:subdivision:12"
//	ScopedTopic("sales-goals", "subdivision", "12", "agent", "348") // "sales-goals:subdivision:12:agent:348"
//	ScopedTopic("theme", "session", sessionID)                   // "theme:session:<id>"
//
// Parts are written verbatim. Empty parts are preserved as empty segments
// rather than dropped, so a missing scope yields a distinct dead topic
// (ScopedTopic("theme", "session", "") == "theme:session:") instead of
// silently collapsing onto a shorter, real topic. Callers own canonicalization:
// parts that already contain a colon are passed through unchanged and produce
// extra segments. There is no escaping or validation.
func ScopedTopic(base string, parts ...string) string {
	if len(parts) == 0 {
		return base
	}
	n := len(base) + len(parts)
	for _, p := range parts {
		n += len(p)
	}
	var b strings.Builder
	b.Grow(n)
	b.WriteString(base)
	for _, p := range parts {
		b.WriteByte(':')
		b.WriteString(p)
	}
	return b.String()
}
