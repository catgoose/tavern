package templ

import (
	"context"
	"errors"
	"io"
	"testing"

	atempl "github.com/a-h/templ"
	"github.com/stretchr/testify/require"
)

func textComponent(s string) atempl.Component {
	return atempl.ComponentFunc(func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	})
}

func failComponent() atempl.Component {
	return atempl.ComponentFunc(func(_ context.Context, _ io.Writer) error {
		return errors.New("render failed")
	})
}

func TestReplaceComponent(t *testing.T) {
	f := ReplaceComponent("stats", textComponent("<span>42</span>"))
	require.Equal(t, "stats", f.ID)
	require.Equal(t, "outerHTML", f.Swap)
	require.Equal(t, "<span>42</span>", f.HTML)
}

func TestAppendComponent(t *testing.T) {
	f := AppendComponent("list", textComponent("<li>new</li>"))
	require.Equal(t, "list", f.ID)
	require.Equal(t, "beforeend", f.Swap)
	require.Equal(t, "<li>new</li>", f.HTML)
}

func TestPrependComponent(t *testing.T) {
	f := PrependComponent("list", textComponent("<li>first</li>"))
	require.Equal(t, "list", f.ID)
	require.Equal(t, "afterbegin", f.Swap)
	require.Equal(t, "<li>first</li>", f.HTML)
}

func TestRenderError(t *testing.T) {
	f := ReplaceComponent("broken", failComponent())
	require.Contains(t, f.HTML, "render error")
	require.Contains(t, f.HTML, "render failed")
}
