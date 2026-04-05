package tavern

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
)

// Component renders itself to a writer. This interface is intentionally
// identical to templ.Component so that templ components can be passed
// directly without tavern importing the templ package.
type Component interface {
	Render(ctx context.Context, w io.Writer) error
}

// ReplaceComponent renders a Component and returns a Replace fragment.
// If rendering fails, the fragment contains the error message as an HTML comment.
func ReplaceComponent(id string, cmp Component) Fragment {
	return Fragment{ID: id, Swap: "outerHTML", HTML: RenderComponent(cmp)}
}

// AppendComponent renders a Component and returns an Append fragment.
// If rendering fails, the fragment contains the error message as an HTML comment.
func AppendComponent(id string, cmp Component) Fragment {
	return Fragment{ID: id, Swap: "beforeend", HTML: RenderComponent(cmp)}
}

// PrependComponent renders a Component and returns a Prepend fragment.
// If rendering fails, the fragment contains the error message as an HTML comment.
func PrependComponent(id string, cmp Component) Fragment {
	return Fragment{ID: id, Swap: "afterbegin", HTML: RenderComponent(cmp)}
}

// RenderComponent renders a Component to a string.
// If rendering fails, it returns an HTML comment containing the escaped error message.
func RenderComponent(cmp Component) string {
	var buf bytes.Buffer
	if err := cmp.Render(context.Background(), &buf); err != nil {
		return "<!-- render error: " + escapeAttr(err.Error()) + " -->"
	}
	return buf.String()
}

// escapeAttr escapes characters that are unsafe in HTML attribute values.
func escapeAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// Fragment describes a targeted DOM mutation for HTMX OOB swaps via SSE.
type Fragment struct {
	ID   string // target element ID
	Swap string // hx-swap-oob value: outerHTML, innerHTML, delete, beforeend, afterbegin
	HTML string // inner HTML content (empty for delete)
}

// Delete creates a fragment that removes an element from the DOM.
func Delete(id string) Fragment {
	return Fragment{ID: id, Swap: "delete"}
}

// Replace creates a fragment that replaces an element's outer HTML.
func Replace(id, html string) Fragment {
	return Fragment{ID: id, Swap: "outerHTML", HTML: html}
}

// Append creates a fragment that appends content to the end of an element.
func Append(id, html string) Fragment {
	return Fragment{ID: id, Swap: "beforeend", HTML: html}
}

// Prepend creates a fragment that prepends content to the beginning of an element.
func Prepend(id, html string) Fragment {
	return Fragment{ID: id, Swap: "afterbegin", HTML: html}
}

// RenderFragments concatenates fragments into a single SSE-ready HTML string.
// Each fragment is wrapped with hx-swap-oob for HTMX OOB processing.
func RenderFragments(fragments ...Fragment) string {
	var b strings.Builder
	for _, f := range fragments {
		if f.Swap == "delete" {
			fmt.Fprintf(&b, `<div id="%s" hx-swap-oob="delete"></div>`, escapeAttr(f.ID))
		} else {
			fmt.Fprintf(&b, `<div id="%s" hx-swap-oob="%s">%s</div>`, escapeAttr(f.ID), f.Swap, f.HTML)
		}
	}
	return b.String()
}

// PublishOOB renders the given fragments and publishes them as a single SSE event.
func (b *SSEBroker) PublishOOB(topic string, fragments ...Fragment) {
	b.Publish(topic, RenderFragments(fragments...))
}

// PublishOOBTo renders the given fragments and publishes them only to scoped
// subscribers whose scope matches.
func (b *SSEBroker) PublishOOBTo(topic, scope string, fragments ...Fragment) {
	b.PublishTo(topic, scope, RenderFragments(fragments...))
}

// PublishIfChangedOOB renders the given fragments and publishes the result
// to the topic only if it differs from the last message published via
// [SSEBroker.PublishIfChanged] for that topic. Returns true if published
// (content changed), false if skipped (identical).
func (b *SSEBroker) PublishIfChangedOOB(topic string, fragments ...Fragment) bool {
	msg := NewSSEMessage("oob", RenderFragments(fragments...)).String()
	return b.PublishIfChanged(topic, msg)
}

// PublishIfChangedOOBTo renders the given fragments and publishes the result
// only to scoped subscribers of the topic whose scope matches, and only if
// the content differs from the last publish for that topic+scope. Returns
// true if published, false if skipped.
func (b *SSEBroker) PublishIfChangedOOBTo(topic, scope string, fragments ...Fragment) bool {
	msg := NewSSEMessage("oob", RenderFragments(fragments...)).String()
	return b.PublishIfChangedTo(topic, scope, msg)
}

// RenderComponentErr renders a Component to a string, returning the error
// separately instead of embedding it in an HTML comment. This is useful when
// you want to handle render errors explicitly rather than silently embedding
// them in the output.
func RenderComponentErr(cmp Component) (string, error) {
	var buf bytes.Buffer
	if err := cmp.Render(context.Background(), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// PublishLazyOOB calls renderFn only if the topic has subscribers, then
// publishes the rendered fragments. This avoids expensive rendering (DB
// queries, template execution) when nobody is listening. If renderFn returns
// no fragments, no message is published.
func (b *SSEBroker) PublishLazyOOB(topic string, renderFn func() []Fragment) {
	if !b.HasSubscribers(topic) {
		return
	}
	fragments := renderFn()
	if len(fragments) == 0 {
		return
	}
	b.PublishOOB(topic, fragments...)
}

// PublishLazyIfChangedOOB calls renderFn only if the topic has subscribers,
// then publishes the rendered fragments only if the content differs from the
// last publish. Combines the subscriber guard with deduplication. Returns true
// if published (content changed), false if skipped (no subscribers, no
// fragments, or identical content).
func (b *SSEBroker) PublishLazyIfChangedOOB(topic string, renderFn func() []Fragment) bool {
	if !b.HasSubscribers(topic) {
		return false
	}
	fragments := renderFn()
	if len(fragments) == 0 {
		return false
	}
	return b.PublishIfChangedOOB(topic, fragments...)
}

// PublishLazyOOBTo calls renderFn only if the topic has subscribers, then
// publishes the rendered fragments to scoped subscribers matching the scope.
func (b *SSEBroker) PublishLazyOOBTo(topic, scope string, renderFn func() []Fragment) {
	if !b.HasSubscribers(topic) {
		return
	}
	fragments := renderFn()
	if len(fragments) == 0 {
		return
	}
	b.PublishOOBTo(topic, scope, fragments...)
}

// PublishLazyIfChangedOOBTo calls renderFn only if the topic has subscribers,
// then publishes the rendered fragments to scoped subscribers only if the
// content differs from the last publish for that topic+scope.
func (b *SSEBroker) PublishLazyIfChangedOOBTo(topic, scope string, renderFn func() []Fragment) bool {
	if !b.HasSubscribers(topic) {
		return false
	}
	fragments := renderFn()
	if len(fragments) == 0 {
		return false
	}
	return b.PublishIfChangedOOBTo(topic, scope, fragments...)
}
