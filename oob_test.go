package tavern

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDelete(t *testing.T) {
	f := Delete("task-row-5")
	assert.Equal(t, "task-row-5", f.ID)
	assert.Equal(t, "delete", f.Swap)
	assert.Empty(t, f.HTML)
}

func TestReplace(t *testing.T) {
	f := Replace("stats-bar", "<span>3</span>")
	assert.Equal(t, "stats-bar", f.ID)
	assert.Equal(t, "outerHTML", f.Swap)
	assert.Equal(t, "<span>3</span>", f.HTML)
}

func TestAppend(t *testing.T) {
	f := Append("list", "<li>new</li>")
	assert.Equal(t, "beforeend", f.Swap)
}

func TestPrepend(t *testing.T) {
	f := Prepend("feed", "<div>latest</div>")
	assert.Equal(t, "afterbegin", f.Swap)
}

func TestRenderFragments(t *testing.T) {
	html := RenderFragments(
		Delete("task-row-5"),
		Replace("stats-bar", "<span>2</span>"),
	)
	assert.Contains(t, html, `id="task-row-5" hx-swap-oob="delete"`)
	assert.Contains(t, html, `id="stats-bar" hx-swap-oob="outerHTML"`)
	assert.Contains(t, html, "<span>2</span>")
}

// testComponent implements Component for testing.
type testComponent struct {
	html string
	err  error
}

func (c testComponent) Render(_ context.Context, w io.Writer) error {
	if c.err != nil {
		return c.err
	}
	_, err := io.WriteString(w, c.html)
	return err
}

func TestReplaceComponent(t *testing.T) {
	f := ReplaceComponent("stats", testComponent{html: "<span>42</span>"})
	assert.Equal(t, "stats", f.ID)
	assert.Equal(t, "outerHTML", f.Swap)
	assert.Equal(t, "<span>42</span>", f.HTML)
}

func TestAppendComponent(t *testing.T) {
	f := AppendComponent("list", testComponent{html: "<li>new</li>"})
	assert.Equal(t, "list", f.ID)
	assert.Equal(t, "beforeend", f.Swap)
	assert.Equal(t, "<li>new</li>", f.HTML)
}

func TestPrependComponent(t *testing.T) {
	f := PrependComponent("list", testComponent{html: "<li>first</li>"})
	assert.Equal(t, "list", f.ID)
	assert.Equal(t, "afterbegin", f.Swap)
	assert.Equal(t, "<li>first</li>", f.HTML)
}

func TestRenderComponentError(t *testing.T) {
	f := ReplaceComponent("broken", testComponent{err: errors.New("render failed")})
	assert.Contains(t, f.HTML, "render error")
	assert.Contains(t, f.HTML, "render failed")
}
