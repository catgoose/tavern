package tavern

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

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

func TestRenderComponent(t *testing.T) {
	cmp := testComponent{html: "<span>hello</span>"}
	result := RenderComponent(cmp)
	assert.Equal(t, "<span>hello</span>", result)
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

func TestPublishOOB_DeliveredToSubscribers(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("updates")
	defer unsub()

	b.PublishOOB("updates", Replace("status", "<b>online</b>"))

	select {
	case msg := <-ch:
		assert.Contains(t, msg, `hx-swap-oob="outerHTML"`)
		assert.Contains(t, msg, `id="status"`)
		assert.Contains(t, msg, "<b>online</b>")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OOB message")
	}
}

func TestPublishOOB_MultipleFragments(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("updates")
	defer unsub()

	b.PublishOOB("updates",
		Replace("box-a", "<p>A</p>"),
		Delete("box-b"),
	)

	select {
	case msg := <-ch:
		assert.Contains(t, msg, `id="box-a"`)
		assert.Contains(t, msg, "<p>A</p>")
		assert.Contains(t, msg, `id="box-b" hx-swap-oob="delete"`)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for OOB message")
	}
}

func TestPublishOOB_NoSubscribers_NoOp(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	// Should not panic when there are no subscribers.
	assert.NotPanics(t, func() {
		b.PublishOOB("nonexistent", Replace("el", "<div/>"))
	})
}

func TestRenderFragments_EscapesID(t *testing.T) {
	// ID with quotes and angle brackets must be escaped in the attribute.
	html := RenderFragments(Replace(`bad"<id>`, "<p>content</p>"))
	assert.Contains(t, html, `id="bad&quot;&lt;id&gt;"`)
	assert.NotContains(t, html, `id="bad"<id>"`)

	// Delete variant also escapes.
	html2 := RenderFragments(Delete(`x"y`))
	assert.Contains(t, html2, `id="x&quot;y"`)
}

func TestPublishIfChangedOOB_PublishesOnChange(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	ok := b.PublishIfChangedOOB("t", Replace("el", "<b>v1</b>"))
	assert.True(t, ok)

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "v1")
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestPublishIfChangedOOB_SkipsDuplicate(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.Subscribe("t")
	defer unsub()

	ok1 := b.PublishIfChangedOOB("t", Replace("el", "<b>v1</b>"))
	assert.True(t, ok1)
	<-ch // drain

	ok2 := b.PublishIfChangedOOB("t", Replace("el", "<b>v1</b>"))
	assert.False(t, ok2)

	select {
	case <-ch:
		t.Fatal("should not have received duplicate")
	default:
	}
}

func TestPublishIfChangedOOBTo_ScopedDelivery(t *testing.T) {
	b := NewSSEBroker()
	defer b.Close()

	ch, unsub := b.SubscribeScoped("t", "user-1")
	defer unsub()

	ok := b.PublishIfChangedOOBTo("t", "user-1", Replace("el", "<b>scoped</b>"))
	assert.True(t, ok)

	select {
	case msg := <-ch:
		assert.Contains(t, msg, "scoped")
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}

func TestRenderComponentError_EscapesComment(t *testing.T) {
	// An error message containing --> must not break out of the HTML comment.
	// escapeAttr replaces > with &gt;, turning --> into --&gt; inside the comment body.
	cmp := testComponent{err: errors.New("oops --> injected")}
	f := ReplaceComponent("el", cmp)
	// The escaped payload must not contain the raw --> sequence.
	assert.NotContains(t, f.HTML, "oops -->", "raw --> from error payload must be escaped")
	assert.Contains(t, f.HTML, "--&gt;", "closing angle bracket in error must be escaped")
	assert.Contains(t, f.HTML, "oops")
}
