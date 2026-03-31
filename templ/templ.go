// Package templ provides templ.Component-aware Fragment helpers for tavern.
package templ

import (
	"bytes"
	"context"

	atempl "github.com/a-h/templ"
	"github.com/catgoose/tavern"
)

// ReplaceComponent renders a templ.Component and returns a Replace fragment.
// If rendering fails, the fragment contains the error message as HTML.
func ReplaceComponent(id string, cmp atempl.Component) tavern.Fragment {
	return tavern.Fragment{ID: id, Swap: "outerHTML", HTML: render(cmp)}
}

// AppendComponent renders a templ.Component and returns an Append fragment.
// If rendering fails, the fragment contains the error message as HTML.
func AppendComponent(id string, cmp atempl.Component) tavern.Fragment {
	return tavern.Fragment{ID: id, Swap: "beforeend", HTML: render(cmp)}
}

// PrependComponent renders a templ.Component and returns a Prepend fragment.
// If rendering fails, the fragment contains the error message as HTML.
func PrependComponent(id string, cmp atempl.Component) tavern.Fragment {
	return tavern.Fragment{ID: id, Swap: "afterbegin", HTML: render(cmp)}
}

func render(cmp atempl.Component) string {
	var buf bytes.Buffer
	if err := cmp.Render(context.Background(), &buf); err != nil {
		return "<!-- render error: " + err.Error() + " -->"
	}
	return buf.String()
}
