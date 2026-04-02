# SSE + HTMX Recipe Cookbook

Practical patterns for wiring tavern's pub/sub broker to HTMX frontends over
Server-Sent Events. Each recipe shows the Go handler, the OOB fragment
construction, the HTML/templ markup, and (where non-obvious) the client-side
EventSource setup.

All examples assume an `*tavern.SSEBroker` named `broker` is available to
handlers and that the SSE endpoint writes messages from `broker.Subscribe` to
the response as shown in the [README](README.md#echo-sse-endpoint).

---

## 1. Remove row and update dashboard tiles

Delete a record from the database, then publish a single SSE event that removes
the table row and updates summary tiles in one atomic OOB swap.

### Go handler

```go
func deleteProject(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        id := c.Param("id")
        if _, err := db.ExecContext(c.Request().Context(),
            "DELETE FROM projects WHERE id = ?", id); err != nil {
            return err
        }

        count, _ := countProjects(db)
        broker.PublishOOB("projects",
            tavern.Delete("project-"+id),
            tavern.Replace("project-count",
                fmt.Sprintf(`<span id="project-count">%d</span>`, count)),
            tavern.Replace("project-status",
                fmt.Sprintf(`<span id="project-status">%d active</span>`, count)),
        )

        return c.NoContent(http.StatusOK)
    }
}
```

### HTML (OOB targets)

```html
<table id="project-table">
  <tr id="project-42">
    <td>Acme</td>
    <td>
      <button hx-delete="/projects/42" hx-swap="none">Delete</button>
    </td>
  </tr>
</table>

<div id="dashboard-tiles">
  <span id="project-count">5</span>
  <span id="project-status">5 active</span>
</div>
```

### SSE connection

```html
<div hx-ext="sse" sse-connect="/sse?topic=projects" sse-swap="message">
</div>
```

---

## 2. Inline row edit with OOB side-effects

Save an inline edit via HTMX, swap the updated row directly from the HTTP
response, then fire OOB updates to breadcrumbs and a status bar via SSE so
every connected client sees the change.

### Go handler

```go
func updatePerson(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        id := c.Param("id")
        name := c.FormValue("name")
        if _, err := db.ExecContext(c.Request().Context(),
            "UPDATE people SET name = ? WHERE id = ?", name, id); err != nil {
            return err
        }

        // Direct HTMX response swaps the edited row.
        row := fmt.Sprintf(`<tr id="person-%s"><td>%s</td></tr>`, id, name)

        // SSE pushes OOB updates to all other clients.
        broker.PublishOOB("people",
            tavern.Replace("person-"+id,
                fmt.Sprintf(`<tr id="person-%s"><td>%s</td></tr>`, id, name)),
            tavern.Replace("breadcrumb-name",
                fmt.Sprintf(`<span id="breadcrumb-name">%s</span>`, name)),
            tavern.Replace("status-bar",
                fmt.Sprintf(`<div id="status-bar">Last edit: %s</div>`, name)),
        )

        return c.HTML(http.StatusOK, row)
    }
}
```

### HTML (OOB targets)

```html
<nav>
  <span id="breadcrumb-name">Alice</span>
</nav>

<table>
  <tr id="person-7">
    <td>
      <form hx-put="/people/7" hx-target="#person-7" hx-swap="outerHTML">
        <input name="name" value="Alice" />
        <button type="submit">Save</button>
      </form>
    </td>
  </tr>
</table>

<div id="status-bar">Last edit: --</div>
```

---

## 3. Multi-topic fan-in SSE stream

A single EventSource can subscribe to multiple topics by merging them into one
SSE connection with a fan-in select loop. Each message is tagged with the topic
name as the SSE event type.

### Go handler

```go
func multiTopicSSE(broker *tavern.SSEBroker) echo.HandlerFunc {
    return func(c echo.Context) error {
        c.Response().Header().Set("Content-Type", "text/event-stream")
        c.Response().Header().Set("Cache-Control", "no-cache")
        c.Response().Header().Set("Connection", "keep-alive")

        topics := []string{"projects", "people", "activity"}
        channels := make([]<-chan string, len(topics))
        for i, t := range topics {
            ch, unsub := broker.Subscribe(t)
            defer unsub()
            channels[i] = ch
        }

        for {
            // Fan-in: reflect.Select or manual select for a fixed set.
            for i, ch := range channels {
                select {
                case msg, ok := <-ch:
                    if !ok {
                        return nil
                    }
                    sse := tavern.NewSSEMessage(topics[i], msg).String()
                    if _, err := fmt.Fprint(c.Response(), sse); err != nil {
                        return nil
                    }
                    c.Response().Flush()
                default:
                }
            }

            // Wait on any channel or client disconnect.
            select {
            case <-c.Request().Context().Done():
                return nil
            default:
            }
        }
    }
}
```

> **Note:** For production code with a dynamic number of topics, use
> `reflect.Select` to build the cases at runtime. The busy-loop above is
> simplified for clarity; a `reflect.Select`-based implementation avoids
> spinning.

### Go handler (reflect.Select version)

```go
import "reflect"

func multiTopicSSEReflect(broker *tavern.SSEBroker) echo.HandlerFunc {
    return func(c echo.Context) error {
        c.Response().Header().Set("Content-Type", "text/event-stream")
        c.Response().Header().Set("Cache-Control", "no-cache")
        c.Response().Header().Set("Connection", "keep-alive")

        topics := []string{"projects", "people", "activity"}
        cases := make([]reflect.SelectCase, len(topics)+1)
        for i, t := range topics {
            ch, unsub := broker.Subscribe(t)
            defer unsub()
            cases[i] = reflect.SelectCase{
                Dir:  reflect.SelectRecv,
                Chan: reflect.ValueOf(ch),
            }
        }
        // Last case: context cancellation.
        cases[len(topics)] = reflect.SelectCase{
            Dir:  reflect.SelectRecv,
            Chan: reflect.ValueOf(c.Request().Context().Done()),
        }

        for {
            chosen, value, ok := reflect.Select(cases)
            if chosen == len(topics) || !ok {
                return nil // client disconnected or channel closed
            }
            sse := tavern.NewSSEMessage(topics[chosen], value.String()).String()
            if _, err := fmt.Fprint(c.Response(), sse); err != nil {
                return nil
            }
            c.Response().Flush()
        }
    }
}
```

### Client JS

```js
const es = new EventSource("/sse/multi");

es.addEventListener("projects", (e) => {
  // e.data contains OOB HTML fragments for the projects topic.
  document.getElementById("projects-stream").innerHTML += e.data;
});

es.addEventListener("people", (e) => {
  document.getElementById("people-stream").innerHTML += e.data;
});

es.addEventListener("activity", (e) => {
  document.getElementById("activity-stream").innerHTML += e.data;
});
```

> When using HTMX's SSE extension, set `sse-swap` to the event name to
> automatically route each topic's fragments to the correct swap target.

---

## 4. Scoped publishing (per-user / per-resource)

Use `SubscribeScoped` and `PublishTo` to deliver events only to clients
watching a specific resource or logged in as a specific user. Other subscribers
on the same topic with a different scope never see the message.

### Go handler (subscribe)

```go
func scopedSSE(broker *tavern.SSEBroker) echo.HandlerFunc {
    return func(c echo.Context) error {
        c.Response().Header().Set("Content-Type", "text/event-stream")
        c.Response().Header().Set("Cache-Control", "no-cache")
        c.Response().Header().Set("Connection", "keep-live")

        userID := c.Get("user_id").(string) // from auth middleware
        ch, unsub := broker.SubscribeScoped("notifications", userID)
        defer unsub()

        for {
            select {
            case msg, ok := <-ch:
                if !ok {
                    return nil
                }
                sse := tavern.NewSSEMessage("notification", msg).String()
                if _, err := fmt.Fprint(c.Response(), sse); err != nil {
                    return nil
                }
                c.Response().Flush()
            case <-c.Request().Context().Done():
                return nil
            }
        }
    }
}
```

### Go handler (publish)

```go
func assignTask(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        assigneeID := c.FormValue("assignee_id")
        taskName := c.FormValue("name")
        // ... persist to DB ...

        // Only the assignee's SSE connection receives this.
        broker.PublishOOBTo("notifications", assigneeID,
            tavern.Append("notification-list",
                fmt.Sprintf(`<li id="notif-%s">New task: %s</li>`,
                    assigneeID, taskName)),
        )

        return c.NoContent(http.StatusOK)
    }
}
```

### HTML

```html
<div hx-ext="sse" sse-connect="/sse/notifications" sse-swap="notification">
  <ul id="notification-list">
    <!-- new notifications appear here via OOB append -->
  </ul>
</div>
```

---

## 5. OOB templ component swap

Use `tavern.ReplaceComponent` to render a full `templ.Component` as an OOB
replacement fragment. This keeps your server-rendered markup in `.templ` files
instead of scattering HTML strings through handler code.

### templ component (`components/stats.templ`)

```templ
package components

templ StatsCard(label string, value int) {
    <div id="stats-card" class="card">
        <h3>{ label }</h3>
        <p>{ fmt.Sprintf("%d", value) }</p>
    </div>
}
```

### Go handler

```go
import (
    "github.com/catgoose/tavern"
    "myapp/components"
)

func refreshStats(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        count, _ := countActiveUsers(db)

        broker.PublishOOB("dashboard",
            tavern.ReplaceComponent("stats-card",
                components.StatsCard("Active Users", count)),
        )

        return c.NoContent(http.StatusOK)
    }
}
```

### HTML

```html
<div hx-ext="sse" sse-connect="/sse?topic=dashboard" sse-swap="message">
</div>

<!-- Initial render from templ, then replaced via SSE OOB -->
<div id="stats-card" class="card">
  <h3>Active Users</h3>
  <p>12</p>
</div>
```

---

## 6. Optimistic delete with SSE confirmation

The client removes a row immediately on click (optimistic UI). The server
processes the delete and publishes an SSE event so all other connected clients
also remove the row. If the delete fails, the server publishes a rollback
fragment to restore the row.

### Go handler

```go
func optimisticDelete(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        id := c.Param("id")

        // Respond immediately so the initiating client can remove the row.
        // Use 200 to signal the client-side hx-delete succeeded.
        err := db.QueryRowContext(c.Request().Context(),
            "DELETE FROM items WHERE id = ? RETURNING name", id).Err()
        if err != nil {
            // Publish rollback: restore the row for the optimistic client.
            broker.PublishOOB("items",
                tavern.Replace("item-"+id,
                    fmt.Sprintf(`<tr id="item-%s"><td>Error: could not delete</td></tr>`, id)),
            )
            return c.NoContent(http.StatusInternalServerError)
        }

        // Publish confirmed delete to all clients (including the initiator,
        // which already removed it -- the delete OOB swap is idempotent).
        broker.PublishOOB("items",
            tavern.Delete("item-"+id),
        )

        return c.NoContent(http.StatusOK)
    }
}
```

### HTML

```html
<table>
  <tbody hx-ext="sse" sse-connect="/sse?topic=items" sse-swap="message">
    <tr id="item-99">
      <td>Widget</td>
      <td>
        <!-- hx-on removes the row immediately (optimistic) -->
        <button
          hx-delete="/items/99"
          hx-swap="none"
          hx-on::before-request="this.closest('tr').remove()"
        >
          Delete
        </button>
      </td>
    </tr>
  </tbody>
</table>
```

---

## 7. Append to a list (live feed)

Push new entries to a live activity feed on all connected clients. Uses
`tavern.Append` for raw HTML or `tavern.AppendComponent` for templ components.

### Go handler (raw HTML)

```go
func createComment(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        body := c.FormValue("body")
        author := c.Get("user_name").(string)
        id, _ := insertComment(db, author, body)

        broker.PublishOOB("comments",
            tavern.Append("comment-feed",
                fmt.Sprintf(
                    `<div id="comment-%d" class="comment"><strong>%s</strong>: %s</div>`,
                    id, author, body,
                )),
        )

        return c.NoContent(http.StatusCreated)
    }
}
```

### Go handler (templ component)

```go
import "github.com/catgoose/tavern"

func createCommentTempl(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        body := c.FormValue("body")
        author := c.Get("user_name").(string)
        id, _ := insertComment(db, author, body)

        broker.PublishOOB("comments",
            tavern.AppendComponent("comment-feed",
                components.CommentEntry(id, author, body)),
        )

        return c.NoContent(http.StatusCreated)
    }
}
```

### HTML

```html
<form hx-post="/comments" hx-swap="none">
  <textarea name="body"></textarea>
  <button type="submit">Post</button>
</form>

<div id="comment-feed"
     hx-ext="sse"
     sse-connect="/sse?topic=comments"
     sse-swap="message">
  <!-- new comments appended here via OOB -->
</div>
```

### Client JS (non-HTMX fallback)

```js
const es = new EventSource("/sse?topic=comments");

es.addEventListener("message", (e) => {
  const feed = document.getElementById("comment-feed");
  feed.insertAdjacentHTML("beforeend", e.data);
});
```
