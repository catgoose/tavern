package tavern

import (
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
