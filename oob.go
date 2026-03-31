package tavern

import (
	"fmt"
	"strings"
)

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
			fmt.Fprintf(&b, `<div id="%s" hx-swap-oob="delete"></div>`, f.ID)
		} else {
			fmt.Fprintf(&b, `<div id="%s" hx-swap-oob="%s">%s</div>`, f.ID, f.Swap, f.HTML)
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
