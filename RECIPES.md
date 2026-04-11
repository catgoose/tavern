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
SSE connection. Each message is tagged with the topic name as the SSE event
type.

### Go handler (SubscribeMulti)

`SubscribeMulti` is the preferred way to fan-in multiple topics onto a single
channel. It returns `TopicMessage` values so you always know which topic each
message came from.

```go
func multiTopicSSE(broker *tavern.SSEBroker) echo.HandlerFunc {
    return func(c echo.Context) error {
        c.Response().Header().Set("Content-Type", "text/event-stream")
        c.Response().Header().Set("Cache-Control", "no-cache")
        c.Response().Header().Set("Connection", "keep-alive")

        ch, unsub := broker.SubscribeMulti("projects", "people", "activity")
        defer unsub()

        for {
            select {
            case msg, ok := <-ch:
                if !ok {
                    return nil
                }
                sse := tavern.NewSSEMessage(msg.Topic, msg.Data).String()
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

### Go handler (SubscribeMultiWith -- composable options)

Use `SubscribeMultiWith` to layer filtering, rate limiting, or other options
across all topics:

```go
func multiTopicFilteredSSE(broker *tavern.SSEBroker) echo.HandlerFunc {
    return func(c echo.Context) error {
        c.Response().Header().Set("Content-Type", "text/event-stream")
        c.Response().Header().Set("Cache-Control", "no-cache")
        c.Response().Header().Set("Connection", "keep-alive")

        // With filtering and rate limiting across all topics:
        ch, unsub := broker.SubscribeMultiWith(
            []string{"projects", "people", "activity"},
            tavern.SubWithFilter(func(msg string) bool {
                return strings.Contains(msg, "important")
            }),
            tavern.SubWithRate(tavern.Rate{MaxPerSecond: 10}),
        )
        defer unsub()

        for {
            select {
            case msg, ok := <-ch:
                if !ok {
                    return nil
                }
                sse := tavern.NewSSEMessage(msg.Topic, msg.Data).String()
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

<details>
<summary>Advanced: manual fan-in with individual Subscribe calls</summary>

> **Preferred:** Use `SubscribeMulti` for multi-topic streams. The manual
> patterns below are shown for reference when you need custom per-topic
> handling.

#### Busy-loop fan-in (simplified)

```go
func multiTopicSSEManual(broker *tavern.SSEBroker) echo.HandlerFunc {
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

#### reflect.Select version

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

</details>

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

---

## 8. Debounced search with SSE results

Publish search results via SSE only after the user stops typing. The server
debounces rapid keystrokes so only the final query triggers rendering and
publishing.

### Go handler (search endpoint)

```go
func searchHandler(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        query := c.QueryParam("q")
        results := search(db, query)
        html := renderSearchResults(results)

        // Only publish after 300ms of quiet — rapid typing resets the timer.
        broker.PublishDebounced("search-"+userID(c), html, 300*time.Millisecond)

        return c.NoContent(http.StatusOK)
    }
}
```

### HTML

```html
<input type="search"
       hx-get="/search"
       hx-trigger="keyup changed delay:100ms"
       hx-target="#search-results"
       hx-swap="none"
       name="q" />

<div id="search-results"
     hx-ext="sse"
     sse-connect="/sse?topic=search-user123"
     sse-swap="message">
</div>
```

---

## 9. Throttled live dashboard

Rate-limit a high-frequency data source to publish at most once per second.
The first update shows immediately; subsequent updates are coalesced.

### Go publisher

```go
func startMetricsPublisher(ctx context.Context, broker *tavern.SSEBroker) {
    broker.RunPublisher(ctx, func(ctx context.Context) {
        for {
            select {
            case <-ctx.Done():
                return
            case sample := <-metricsChannel:
                html := renderMetricsPanel(sample)
                // At most 1 publish per second, latest data wins.
                broker.PublishThrottled("metrics", html, time.Second)
            }
        }
    })
}
```

### HTML

```html
<div id="metrics-panel"
     hx-ext="sse"
     sse-connect="/sse?topic=metrics"
     sse-swap="message">
  <!-- updates at most once per second -->
</div>
```

---

## 10. Activity feed with replay history

New visitors to the activity page immediately see the last 20 events, then
receive live updates as they happen.

### Go setup

```go
broker.SetReplayPolicy("activity", 20)

func createEvent(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        event := persistEvent(db, c)
        html := renderActivityItem(event)

        // Cached for replay + published to live subscribers
        broker.PublishWithReplay("activity", html)

        return c.NoContent(http.StatusCreated)
    }
}
```

### HTML

```html
<ul id="activity-feed"
    hx-ext="sse"
    sse-connect="/sse?topic=activity"
    sse-swap="message">
  <!-- last 20 events replayed on connect, then live updates -->
</ul>
```

---

## 11. Resumable SSE with Last-Event-ID

Use `PublishWithID` and `SubscribeFromID` for gap-free reconnection. When
a client's connection drops, the browser automatically sends the last
received event ID on reconnect. The server replays only the missed messages.

### Go setup

```go
broker.SetReplayPolicy("notifications", 50) // keep last 50 for resumption

var eventSeq atomic.Int64

func publishNotification(broker *tavern.SSEBroker, payload string) {
    id := fmt.Sprintf("evt-%d", eventSeq.Add(1))
    msg := tavern.NewSSEMessage("notification", payload).WithID(id).String()
    broker.PublishWithID("notifications", id, msg)
}
```

### Go handler (SSE endpoint)

```go
func sseHandler(broker *tavern.SSEBroker) echo.HandlerFunc {
    return func(c echo.Context) error {
        c.Response().Header().Set("Content-Type", "text/event-stream")
        c.Response().Header().Set("Cache-Control", "no-cache")
        c.Response().Header().Set("Connection", "keep-alive")

        lastID := c.Request().Header.Get("Last-Event-ID")
        ch, unsub := broker.SubscribeFromID("notifications", lastID)
        defer unsub()

        for {
            select {
            case msg, ok := <-ch:
                if !ok {
                    return nil
                }
                if _, err := fmt.Fprint(c.Response(), msg); err != nil {
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

### HTML

```html
<div hx-ext="sse" sse-connect="/sse/notifications" sse-swap="notification">
  <!-- EventSource handles Last-Event-ID automatically on reconnect -->
</div>
```

> **Tip:** Set the replay policy large enough to cover typical disconnection
> windows (e.g., 30 seconds of messages). If the client is offline longer
> than the buffer covers, it gets only live messages -- which is usually the
> right trade-off.

> **Tip:** The subscriber buffer size (`WithBufferSize`) limits how many replay
> messages can be queued during reconnect. If you set a large replay policy,
> make sure the buffer is large enough to hold the replay burst — otherwise
> some replayed messages will be dropped silently. For example, with a replay
> policy of 50, use `WithBufferSize(64)` or larger.

> **Note:** `SetReplayGapPolicy` and `OnReplayGap` only take effect when the
> topic uses ID-backed replay (`PublishWithID` or `PublishWithTTL`). Calling
> `SetReplayGapPolicy` on a topic that only uses plain `Publish` has no
> effect — the stream never emits event IDs, so `Last-Event-ID` reconnection
> is not meaningful.

---

## 12. Live dashboard with linkwell navigation

Tavern pushes real-time widget updates via `ScheduledPublisher`. The OOB
fragments include [linkwell](https://github.com/catgoose/linkwell) navigation
controls so breadcrumbs and nav state stay in sync across all connected clients.

> See also: [linkwell RECIPES](https://github.com/catgoose/linkwell/blob/main/RECIPES.md)
> for the navigation setup side.

### Go setup

```go
import (
    "github.com/catgoose/tavern"
    "github.com/catgoose/linkwell"
)

// Register link graph at startup
reg := linkwell.NewRegistry()
reg.Hub("app",
    linkwell.Link{Rel: "dashboard", Href: "/dashboard", Label: "Dashboard"},
    linkwell.Link{Rel: "settings", Href: "/settings", Label: "Settings"},
)

pub := broker.NewScheduledPublisher("dashboard", tavern.WithBaseTick(100*time.Millisecond))

pub.Register("stats", 1*time.Second, func(ctx context.Context, buf *bytes.Buffer) error {
    stats := collectStats()
    // Render stats widget + linkwell breadcrumbs in one OOB batch
    frags := tavern.RenderFragments(
        tavern.ReplaceComponent("stats-panel", views.StatsPanel(stats)),
        tavern.Replace("breadcrumbs",
            renderBreadcrumbs(reg.Breadcrumbs("/dashboard"))),
    )
    buf.WriteString(frags)
    return nil
})

broker.RunPublisher(ctx, pub.Start)
```

### HTML

```html
<nav id="breadcrumbs"><!-- updated via SSE --></nav>

<div id="stats-panel"
     hx-ext="sse"
     sse-connect="/sse?topic=dashboard"
     sse-swap="message">
  <!-- real-time stats + nav in sync -->
</div>
```

---

## 13. Real-time table with linkwell row actions

Tavern broadcasts row updates via SSE. Each row includes
[linkwell](https://github.com/catgoose/linkwell) action controls so the
server decides which actions are available per row — edit, delete, view —
based on permissions and state.

> See also: [linkwell RECIPES](https://github.com/catgoose/linkwell/blob/main/RECIPES.md)
> for action pattern setup.

### Go handler

```go
func updateTask(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        id := c.Param("id")
        task := updateTaskInDB(db, id, c)

        // linkwell determines available actions based on task state
        actions := linkwell.TableRowActions(
            linkwell.Control{Label: "Edit", URL: "/tasks/" + id + "/edit", Method: "GET"},
            linkwell.Control{Label: "Delete", URL: "/tasks/" + id, Method: "DELETE",
                HXConfirm: "Delete this task?"},
        )
        if task.Completed {
            actions = linkwell.TableRowActions(
                linkwell.Control{Label: "Reopen", URL: "/tasks/" + id + "/reopen", Method: "POST"},
            )
        }

        // Broadcast updated row with server-driven actions
        broker.PublishOOB("tasks",
            tavern.ReplaceComponent("task-"+id,
                views.TaskRow(task, actions)),
        )

        return c.NoContent(http.StatusOK)
    }
}
```

### HTML

```html
<table hx-ext="sse" sse-connect="/sse?topic=tasks" sse-swap="message">
  <tbody>
    <tr id="task-42">
      <td>Deploy v2</td>
      <td><!-- linkwell actions rendered server-side --></td>
    </tr>
  </tbody>
</table>
```

---

## 14. Delete broadcast with linkwell error recovery

Tavern broadcasts deletes to all clients. On failure, the rollback fragment
includes [linkwell](https://github.com/catgoose/linkwell) error controls
so the user gets actionable recovery options — not just an error message.

> See also: [linkwell RECIPES](https://github.com/catgoose/linkwell/blob/main/RECIPES.md)
> for error control patterns.

### Go handler

```go
func deleteProject(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        id := c.Param("id")

        if err := db.ExecContext(c.Request().Context(),
            "DELETE FROM projects WHERE id = ?", id).Err(); err != nil {

            // linkwell provides error-specific controls
            errCtx := linkwell.NewErrorContext(http.StatusInternalServerError,
                linkwell.Control{Label: "Retry", URL: "/projects/" + id, Method: "DELETE"},
                linkwell.Control{Label: "Dismiss", HXSwapOOB: "delete"},
            )

            broker.PublishOOB("projects",
                tavern.ReplaceComponent("project-"+id,
                    views.ProjectRowError(id, errCtx)),
            )
            return c.NoContent(http.StatusInternalServerError)
        }

        broker.PublishOOB("projects", tavern.Delete("project-"+id))
        return c.NoContent(http.StatusOK)
    }
}
```

---

## 15. Lifecycle-aware publishing with linkwell controls

Tavern's lifecycle hooks start publishers only when someone is listening.
The published fragments include [linkwell](https://github.com/catgoose/linkwell)
controls that adapt to live state — the server sends both the data and the
available actions in one SSE message.

> See also: [linkwell RECIPES](https://github.com/catgoose/linkwell/blob/main/RECIPES.md)
> for control composition patterns.

### Go setup

```go
var cancelDashboard context.CancelFunc

broker.OnFirstSubscriber("dashboard", func(topic string) {
    ctx, cancel := context.WithCancel(appCtx)
    cancelDashboard = cancel

    broker.RunPublisher(ctx, func(ctx context.Context) {
        ticker := time.NewTicker(time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                stats := collectDashboardStats()

                // Controls change based on live state
                var actions []linkwell.Control
                if stats.AlertCount > 0 {
                    actions = append(actions,
                        linkwell.Control{Label: "View Alerts", URL: "/alerts", Method: "GET"},
                        linkwell.Control{Label: "Acknowledge All", URL: "/alerts/ack", Method: "POST"},
                    )
                }

                broker.PublishLazyIfChangedOOB("dashboard", func() []tavern.Fragment {
                    return []tavern.Fragment{
                        tavern.ReplaceComponent("stats", views.DashboardStats(stats)),
                        tavern.ReplaceComponent("actions", views.DashboardActions(actions)),
                    }
                })
            }
        }
    })
})

broker.OnLastUnsubscribe("dashboard", func(topic string) {
    if cancelDashboard != nil {
        cancelDashboard()
    }
})
```

### HTML

```html
<div hx-ext="sse" sse-connect="/sse?topic=dashboard" sse-swap="message">
  <div id="stats"><!-- live stats --></div>
  <div id="actions"><!-- server-driven actions adapt to state --></div>
</div>
```

---

## 16. Batch publish -- bulk action updating multiple UI regions

When an admin action affects multiple topics, use `Batch` to buffer all
publishes and deliver them as a single write per subscriber. This avoids
intermediate DOM states where some regions are updated and others are stale.

### Go handler

```go
func bulkApproveOrders(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        ids := c.FormValue("ids") // comma-separated order IDs
        orderIDs := strings.Split(ids, ",")

        approvedCount, _ := approveOrders(db, orderIDs)
        stats := fetchOrderStats(db)
        activity := renderRecentActivity(db)

        batch := broker.Batch()
        batch.PublishOOB("orders",
            tavern.Replace("order-count",
                fmt.Sprintf(`<span id="order-count">%d pending</span>`, stats.Pending)))
        batch.PublishOOB("dashboard",
            tavern.ReplaceComponent("order-stats", views.OrderStats(stats)))
        batch.PublishOOB("activity",
            tavern.Append("activity-feed", activity))
        for _, id := range orderIDs {
            batch.PublishOOB("orders",
                tavern.Replace("order-"+id,
                    fmt.Sprintf(`<tr id="order-%s"><td>Approved</td></tr>`, id)))
        }
        batch.Flush() // one atomic write per subscriber

        return c.JSON(http.StatusOK, map[string]int{"approved": approvedCount})
    }
}
```

### HTML

```html
<div hx-ext="sse" sse-connect="/sse?topic=orders" sse-swap="message">
  <span id="order-count">12 pending</span>
  <table>
    <tr id="order-42"><td>Pending</td></tr>
    <tr id="order-43"><td>Pending</td></tr>
  </table>
</div>
```

---

## 17. Topic groups -- single SSE connection for a dashboard

Use `DefineGroup` and `GroupHandler` to stream multiple topics on one SSE
connection. The client receives each topic as a separate SSE event type.

### Go setup

```go
broker.DefineGroup("dashboard", []string{"stats", "alerts", "activity"})

// Standard library
mux.Handle("/sse/dashboard", broker.GroupHandler("dashboard"))

// Dynamic group -- resolve topics per-request for authorization
broker.DynamicGroup("user-dash", func(r *http.Request) []string {
    user := auth.FromContext(r.Context())
    topics := []string{"stats"}
    if user.IsAdmin {
        topics = append(topics, "alerts", "audit-log")
    }
    return topics
})
mux.Handle("/sse/user-dash", broker.DynamicGroupHandler("user-dash"))
```

### Go publisher

```go
// Each topic publishes independently -- the group handler multiplexes them
broker.PublishOOB("stats",
    tavern.ReplaceComponent("stats-panel", views.StatsPanel(stats)))
broker.PublishOOB("alerts",
    tavern.Replace("alert-badge", fmt.Sprintf(`<span id="alert-badge">%d</span>`, alertCount)))
broker.PublishOOB("activity",
    tavern.Append("activity-feed", renderActivityItem(event)))
```

### HTML

```html
<!-- Single SSE connection, multiple event types -->
<div hx-ext="sse" sse-connect="/sse/dashboard">
  <div id="stats-panel" sse-swap="stats"><!-- live stats --></div>
  <div id="alert-badge" sse-swap="alerts">0</div>
  <ul id="activity-feed" sse-swap="activity"><!-- live feed --></ul>
</div>
```

---

## 18. Subscriber filtering -- activity feed per-user

Use `SubscribeWithFilter` to deliver only messages matching a predicate.
The filter runs in the publish path, so messages that don't match are never
sent to the subscriber's channel.

### Go handler

```go
func activitySSE(broker *tavern.SSEBroker) echo.HandlerFunc {
    return func(c echo.Context) error {
        c.Response().Header().Set("Content-Type", "text/event-stream")
        c.Response().Header().Set("Cache-Control", "no-cache")
        c.Response().Header().Set("Connection", "keep-alive")

        userID := c.Get("user_id").(string)

        // Only deliver activity events that mention this user
        ch, unsub := broker.SubscribeWithFilter("activity", func(msg string) bool {
            return strings.Contains(msg, fmt.Sprintf(`data-user="%s"`, userID))
        })
        defer unsub()

        for {
            select {
            case msg, ok := <-ch:
                if !ok {
                    return nil
                }
                if _, err := fmt.Fprint(c.Response(), msg); err != nil {
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

### Go publisher

```go
// Published to all subscribers, but only delivered to matching filters
broker.PublishOOB("activity",
    tavern.Append("activity-feed",
        fmt.Sprintf(`<li data-user="%s">%s approved order #%s</li>`,
            approverID, approverName, orderID)))
```

### HTML

```html
<ul id="activity-feed"
    hx-ext="sse"
    sse-connect="/sse/activity"
    sse-swap="message">
  <!-- only activity relevant to the logged-in user appears -->
</ul>
```

---

## 19. Adaptive backpressure -- mobile vs desktop

Use adaptive backpressure to gracefully degrade for slow clients. Mobile
clients on spotty connections get throttled, then simplified, then
disconnected -- instead of silently dropping messages.

### Go setup

```go
broker := tavern.NewSSEBroker(
    tavern.WithBufferSize(16),
    tavern.WithAdaptiveBackpressure(tavern.AdaptiveBackpressure{
        ThrottleAt:   5,   // deliver every 2nd message after 5 consecutive drops
        SimplifyAt:   15,  // switch to lightweight renderer after 15 drops
        DisconnectAt: 40,  // evict after 40 drops (EventSource auto-reconnects)
    }),
)

// Register a lightweight renderer for the simplify tier
broker.SetSimplifiedRenderer("dashboard", func(msg string) string {
    return `<div id="dashboard" class="simplified">
        <p>Dashboard updating slowly. <a href="/dashboard">Refresh</a></p>
    </div>`
})

// Log tier transitions for monitoring
broker.OnBackpressureTierChange(func(sub *tavern.SubscriberInfo, oldTier, newTier tavern.BackpressureTier) {
    slog.Warn("backpressure tier change",
        "subscriber", sub.ID,
        "topic", sub.Topic,
        "from", oldTier.String(),
        "to", newTier.String(),
    )
})
```

When a subscriber's tier changes, Tavern automatically emits a
`tavern-backpressure` control event over the SSE stream with the new tier,
previous tier, and topic. Clients can listen for this event to show
degradation indicators:

```js
es.addEventListener("tavern-backpressure", (e) => {
    const { tier, previousTier, topic } = JSON.parse(e.data);
    if (tier === "throttle" || tier === "simplify") {
        showDegradationBanner(topic, tier);
    } else if (tier === "normal") {
        hideDegradationBanner(topic);
    }
});
```

### Go publisher

```go
// Publish normally -- the broker handles per-subscriber degradation
broker.PublishOOB("dashboard",
    tavern.ReplaceComponent("charts", views.FullCharts(data)),
)
// Fast clients get full charts, throttled clients get every 2nd update,
// simplified clients get the fallback message, and stuck clients get evicted.
```

---

## 20. TTL messages -- toast notifications that auto-expire

Use `PublishWithTTL` for ephemeral messages that disappear from the replay
cache after a timeout. New subscribers who connect after the TTL don't see
stale toasts.

### Go handler

```go
func createToast(broker *tavern.SSEBroker) echo.HandlerFunc {
    return func(c echo.Context) error {
        msg := c.FormValue("message")
        toastID := fmt.Sprintf("toast-%d", time.Now().UnixNano())

        html := fmt.Sprintf(
            `<div id="%s" class="toast">%s</div>`,
            toastID, msg,
        )

        // Visible for 5 seconds in replay cache, then auto-removed from DOM
        broker.PublishWithTTL("toasts", html, 5*time.Second,
            tavern.WithAutoRemove(toastID),
        )

        return c.NoContent(http.StatusOK)
    }
}
```

### HTML

```html
<div id="toast-area"
     hx-ext="sse"
     sse-connect="/sse?topic=toasts"
     sse-swap="message">
  <!-- toasts appear and auto-delete after TTL expires -->
</div>
```

---

## 21. Reactive hooks -- order approval cascading updates

Use `After` hooks and `OnMutate` to cascade a single business event across
multiple UI regions. When an order is approved, the order table, badge counts,
charts, and activity feed all update without the handler knowing about each
consumer.

### Go setup

```go
// Register mutation handler -- decoupled from specific topics
broker.OnMutate("orders", func(evt tavern.MutationEvent) {
    order := evt.Data.(*Order)

    broker.PublishOOB("order-detail",
        tavern.ReplaceComponent("order-"+order.ID, views.OrderRow(order)))

    stats := fetchOrderStats(db)
    broker.PublishOOB("dashboard",
        tavern.ReplaceComponent("order-stats", views.OrderStats(stats)))
})

// After hook -- when dashboard publishes, update the badge
broker.After("dashboard", func() {
    pending := countPendingOrders(db)
    broker.PublishOOB("nav",
        tavern.Replace("pending-badge",
            fmt.Sprintf(`<span id="pending-badge" class="badge">%d</span>`, pending)))
})

// After hook -- log to activity feed after order-detail updates
broker.After("order-detail", func() {
    broker.PublishOOB("activity",
        tavern.Append("activity-feed",
            fmt.Sprintf(`<li>Order updated at %s</li>`, time.Now().Format("15:04:05"))))
})
```

### Go handler

```go
func approveOrder(broker *tavern.SSEBroker, db *sql.DB) echo.HandlerFunc {
    return func(c echo.Context) error {
        id := c.Param("id")
        order := approveOrderInDB(db, id)

        // One call cascades to order-detail, dashboard, nav badge, activity feed
        broker.NotifyMutate("orders", tavern.MutationEvent{ID: id, Data: order})

        return c.NoContent(http.StatusOK)
    }
}
```

---

## 22. Middleware -- audit logging for admin topics

Use `UseTopics` to add middleware that only runs for specific topic patterns.
This keeps audit logic out of handlers.

### Go setup

```go
broker.UseTopics("admin:*", func(next tavern.PublishFunc) tavern.PublishFunc {
    return func(topic, msg string) {
        slog.Info("admin publish",
            "topic", topic,
            "size", len(msg),
            "time", time.Now().Format(time.RFC3339),
        )
        // Could also persist to an audit table
        insertAuditLog(db, topic, msg)
        next(topic, msg)
    }
})

// Global middleware -- add timing to all publishes
broker.Use(func(next tavern.PublishFunc) tavern.PublishFunc {
    return func(topic, msg string) {
        start := time.Now()
        next(topic, msg)
        slog.Debug("publish latency", "topic", topic, "duration", time.Since(start))
    }
})
```

### Go handler

```go
// Handlers publish normally -- middleware intercepts transparently
broker.Publish("admin:users", renderUserTable())     // audited
broker.Publish("admin:settings", renderSettings())   // audited
broker.Publish("dashboard", renderDashboard())       // not audited
```

---

## 23. Hierarchical topics -- wildcard monitoring dashboard

Use `SubscribeGlob` to monitor an entire topic hierarchy from a single
subscription. An ops dashboard can watch all services without knowing
specific service names.

### Go setup

```go
// Services publish to hierarchical topics
broker.Publish("monitoring/services/api/health", renderHealthBadge("api", healthy))
broker.Publish("monitoring/services/worker/health", renderHealthBadge("worker", degraded))
broker.Publish("monitoring/services/db/health", renderHealthBadge("db", healthy))
broker.Publish("monitoring/alerts/disk-full", renderAlert("disk-full"))
```

### Go handler

```go
func monitoringSSE(broker *tavern.SSEBroker) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")

        // Watch everything under monitoring/
        ch, unsub := broker.SubscribeGlob("monitoring/**")
        defer unsub()

        for {
            select {
            case msg, ok := <-ch:
                if !ok {
                    return
                }
                // msg.Topic tells you which service published
                sse := tavern.NewSSEMessage(msg.Topic, msg.Data).String()
                if _, err := fmt.Fprint(w, sse); err != nil {
                    return
                }
                if f, ok := w.(http.Flusher); ok {
                    f.Flush()
                }
            case <-r.Context().Done():
                return
            }
        }
    }
}
```

### HTML

```html
<div hx-ext="sse" sse-connect="/sse/monitoring">
  <!-- Each service's events arrive with topic as event type -->
  <div sse-swap="monitoring/services/api/health" id="api-health">--</div>
  <div sse-swap="monitoring/services/worker/health" id="worker-health">--</div>
  <div sse-swap="monitoring/services/db/health" id="db-health">--</div>
  <div sse-swap="monitoring/alerts/disk-full" id="disk-alert"></div>
</div>
```

---

## 24. Presence tracking -- who's viewing a document

Use the `presence` subpackage to track which users are viewing a resource
and broadcast presence changes via OOB fragments.

### Go setup

```go
import "github.com/catgoose/tavern/presence"

tracker := presence.New(broker, presence.Config{
    StaleTimeout: 30 * time.Second,
    RenderFunc: func(topic string, users []presence.Info) string {
        var buf strings.Builder
        buf.WriteString(`<ul id="presence-list">`)
        for _, u := range users {
            buf.WriteString(fmt.Sprintf(
                `<li><img src="%s" class="avatar" /> %s</li>`,
                u.Avatar, u.Name,
            ))
        }
        buf.WriteString("</ul>")
        return buf.String()
    },
    OnJoin: func(topic string, info presence.Info) {
        slog.Info("user joined", "topic", topic, "user", info.UserID)
    },
    OnLeave: func(topic string, info presence.Info) {
        slog.Info("user left", "topic", topic, "user", info.UserID)
    },
})
defer tracker.Close()
```

### Go handler

```go
func documentSSE(broker *tavern.SSEBroker, tracker *presence.Tracker) echo.HandlerFunc {
    return func(c echo.Context) error {
        docID := c.Param("id")
        userID := c.Get("user_id").(string)
        userName := c.Get("user_name").(string)
        topic := "doc-" + docID

        tracker.Join(topic, presence.Info{
            UserID: userID,
            Name:   userName,
            Avatar: fmt.Sprintf("/avatars/%s.png", userID),
        })
        defer tracker.Leave(topic, userID)

        // Send heartbeats to keep presence alive
        go func() {
            ticker := time.NewTicker(10 * time.Second)
            defer ticker.Stop()
            for {
                select {
                case <-c.Request().Context().Done():
                    return
                case <-ticker.C:
                    tracker.Heartbeat(topic, userID)
                }
            }
        }()

        // Stream document updates + presence changes
        ch, unsub := broker.Subscribe(topic + ":presence")
        defer unsub()

        return streamSSE(c, ch)
    }
}
```

### HTML

```html
<div hx-ext="sse" sse-connect="/sse/doc/123" sse-swap="message">
  <ul id="presence-list">
    <!-- presence avatars updated via OOB -->
  </ul>
  <div id="doc-content">
    <!-- document content -->
  </div>
</div>
```

---

## 25. Distributed fan-out -- multi-instance deployment

Use the `backend/memory` package to simulate cross-instance message delivery
in tests, or implement the `backend.Backend` interface for production
(Redis pub/sub, NATS, etc.).

### Go setup (test / single-process simulation)

```go
import "github.com/catgoose/tavern/backend/memory"

// Create two linked backends sharing the same message bus
mem := memory.New()
fork := mem.Fork()

// Each broker instance gets its own backend
broker1 := tavern.NewSSEBroker(tavern.WithBackend(mem))
defer broker1.Close()

broker2 := tavern.NewSSEBroker(tavern.WithBackend(fork))
defer broker2.Close()

// Subscribe on broker2
ch, unsub := broker2.Subscribe("events")
defer unsub()

// Publish on broker1 -- broker2 subscribers receive it
broker1.Publish("events", tavern.NewSSEMessage("update", "hello from instance 1").String())

msg := <-ch // "hello from instance 1"
```

### Production pattern

```go
// Implement backend.Backend for your message transport
type redisBackend struct { /* ... */ }

func (r *redisBackend) Publish(ctx context.Context, env backend.MessageEnvelope) error { /* ... */ }
func (r *redisBackend) Subscribe(ctx context.Context, topic string) (<-chan backend.MessageEnvelope, error) { /* ... */ }
func (r *redisBackend) Unsubscribe(topic string) error { /* ... */ }
func (r *redisBackend) Close() error { /* ... */ }

// Wire it up
broker := tavern.NewSSEBroker(tavern.WithBackend(myRedisBackend))
```

The backend interface is minimal: `Publish`, `Subscribe`, `Unsubscribe`, and
`Close`. Messages published on any instance are delivered to subscribers on all
other instances. Scoped publishes work transparently -- the `Scope` field in
`MessageEnvelope` ensures delivery only to matching scoped subscribers.

---

## Recipe 26: Client-Side Connection Awareness with tavern-js

Tavern emits control events over the SSE stream that signal connection state
changes. The [tavern-js](https://github.com/catgoose/tavern-js) companion
library listens for these events and provides declarative UI behaviors.

### Server (Go) -- no changes needed

The broker already emits `tavern-reconnected`, `tavern-replay-gap`, and
`tavern-topics-changed` control events. Just serve your SSE handler as usual:

```go
mux.Handle("/sse/dashboard", broker.SSEHandler("dashboard",
    tavern.WithMaxConnectionDuration(30*time.Minute),
))
```

### Client (HTML + tavern.js)

```html
<script src="https://cdn.jsdelivr.net/gh/catgoose/tavern-js@latest/dist/tavern.min.js"></script>

<div id="dashboard"
     sse-connect="/sse/dashboard"
     sse-swap="update"
     tavern-reconnecting-class="opacity-50 pointer-events-none"
     tavern-gap-action="banner"
     tavern-gap-banner-text="Dashboard was disconnected. Click to refresh.">

  <div tavern-status class="hidden">
    <div class="animate-pulse text-gray-500">Reconnecting...</div>
  </div>

  <!-- Normal SSE content renders here -->
</div>
```

### What happens

1. **Connection drops** -- `htmx:sseError` fires. tavern-js adds `opacity-50`
   and `pointer-events-none` classes, shows the "Reconnecting..." status element.
2. **Browser reconnects** -- `EventSource` auto-reconnects with `Last-Event-ID`.
   tavern sends replay + `tavern-reconnected` event. tavern-js removes the
   classes, hides status.
3. **Replay gap** -- If `Last-Event-ID` has rolled out of the replay log, tavern
   sends `tavern-replay-gap`. tavern-js shows a clickable banner so the user can
   refresh.

### Custom gap handling

For cases where you want programmatic control instead of a banner:

```html
<div id="prices"
     sse-connect="/sse/prices"
     sse-swap="ticker"
     tavern-gap-action="prices-stale">
</div>

<script>
  document.getElementById("prices").addEventListener("prices-stale", () => {
    htmx.trigger("#prices", "htmx:load");
  });
</script>
```

Setting `tavern-gap-action` to a custom name dispatches that as a DOM
event, giving you full control over recovery.

---

## Recipe 27: App-shell lifeline with scoped panel streams

> **Contract reference:** See [docs/stream-contract.md](docs/stream-contract.md)
> for the full lifeline/scoped stream contract -- guarantees, failure isolation,
> reconnection semantics, and a decision guidance table for when to use each
> approach.

One persistent SSE connection for app-level events, with scoped panel streams
that spin up only for high-bandwidth views. Two alternative architectures are
shown below -- choose one, not both.

### Shared setup

```go
broker := tavern.NewSSEBroker(
    tavern.WithBufferSize(32),
    tavern.WithKeepalive(15*time.Second),
)

// Replay for scoped panel topics so reconnect can fill gaps.
broker.SetReplayPolicy("dashboard-data", 50)
broker.SetReplayPolicy("analytics-data", 50)
```

### Approach A: Single connection with AddTopic / RemoveTopic

Best for low-bandwidth panel topics that share the lifeline's lifecycle. See
the [stream contract decision guidance](docs/stream-contract.md#decision-guidance)
for when this is the right choice.

One SSE connection carries everything. The server mutates topic membership
when the user navigates. Requires `SubscribeMultiWithMeta` for a subscriber
ID that `AddTopic` / `RemoveTopic` can target.

#### Lifeline handler

```go
// Custom handler that creates a SubscribeMultiWithMeta subscriber.
func lifelineHandler(broker *tavern.SSEBroker) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        sessionID := r.Header.Get("X-Session-ID")

        ch, unsub := broker.SubscribeMultiWithMeta(
            tavern.SubscribeMeta{ID: sessionID},
            "notifications", "nav-state",
        )
        defer unsub()

        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")
        flusher, _ := w.(http.Flusher)

        for {
            select {
            case msg, ok := <-ch:
                if !ok {
                    return
                }
                fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Topic, msg.Data)
                flusher.Flush()
            case <-r.Context().Done():
                return
            }
        }
    }
}

mux.Handle("/sse/app", lifelineHandler(broker))
```

#### Navigation handler (topic handoff)

```go
// POST /navigate -- add or remove panel topics on the lifeline.
func navigateHandler(broker *tavern.SSEBroker) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        sessionID := r.Header.Get("X-Session-ID")
        panel := r.FormValue("panel")
        action := r.FormValue("action") // "open" or "close"

        topic := panel + "-data"
        switch action {
        case "open":
            broker.AddTopic(sessionID, topic, true)
        case "close":
            broker.RemoveTopic(sessionID, topic, true)
        }
        w.WriteHeader(http.StatusNoContent)
    }
}
```

#### HTML (single connection)

```html
<!-- One persistent SSE connection -- topics change dynamically -->
<div hx-ext="sse" sse-connect="/sse/app">
  <nav id="nav-state" sse-swap="nav-state"></nav>
  <div id="notifications" sse-swap="notifications"></div>
  <!-- Panel swap targets added when tavern-topics-changed arrives -->
  <div id="panel-content" sse-swap="dashboard-data"></div>
</div>
```

### Approach B: Separate scoped connections

Best for high-bandwidth panel topics that need independent backpressure, buffer
sizing, or eviction policies. See the
[stream contract decision guidance](docs/stream-contract.md#decision-guidance)
for when this is the right choice.

The lifeline uses a standard handler for control-plane topics. Each panel opens
its own SSE connection. Works well under HTTP/2 and HTTP/3 where additional
streams are cheap.

#### Handlers

```go
// Lifeline endpoint -- persistent app-shell connection.
broker.DynamicGroup("app-shell", func(r *http.Request) []string {
    return []string{"notifications", "nav-state"}
})
mux.Handle("/sse/app", broker.DynamicGroupHandler("app-shell"))

// High-frequency scoped panel streams with their own handlers.
mux.Handle("/sse/panel/dashboard", broker.SSEHandler("dashboard-data",
    tavern.WithMaxConnectionDuration(10*time.Minute),
))
mux.Handle("/sse/panel/analytics", broker.SSEHandler("analytics-data"))
```

#### HTML (multiple connections)

```html
<!-- Persistent lifeline connection -- never torn down during navigation -->
<div hx-ext="sse" sse-connect="/sse/app">
  <nav id="nav-state" sse-swap="nav-state"></nav>
  <div id="notifications" sse-swap="notifications"></div>
</div>

<!-- Panel content loaded on navigation -->
<main id="panel-content">
  <!-- Loaded via hx-get when user navigates -->
</main>

<!-- Example: dashboard panel (loaded into #panel-content) -->
<template id="dashboard-panel">
  <div hx-ext="sse" sse-connect="/sse/panel/dashboard">
    <div id="chart" sse-swap="dashboard-data"></div>
  </div>
</template>
```

### Publishers (both approaches)

```go
// Control-plane: low-frequency app-shell events.
broker.Publish("notifications", renderNotification(n))
broker.Publish("nav-state", renderBreadcrumb(path))

// Data-plane: high-frequency panel updates with IDs for replay.
// Use PublishTo for user-scoped panels, PublishWithID for replayable streams.
broker.PublishWithID("dashboard-data", eventID, renderChart(data))
broker.PublishTo("analytics-data", userScope, renderMetrics(data))
```

### What happens

1. **Page loads** -- browser opens `/sse/app`. Lifeline delivers notifications
   and nav-state updates.
2. **User clicks "Dashboard"**:
   - *Approach A*: POST to `/navigate` calls `AddTopic` on the lifeline. Client
     receives `tavern-topics-changed` and sets up swap targets.
   - *Approach B*: App loads the dashboard panel via `hx-get`. The panel's
     `sse-connect="/sse/panel/dashboard"` opens a second SSE stream.
3. **Dashboard streams data** -- chart updates flow to the subscriber.
4. **User clicks "Settings"**:
   - *Approach A*: POST to `/navigate` calls `RemoveTopic` for dashboard,
     `AddTopic` for settings.
   - *Approach B*: Dashboard panel removed from DOM, its SSE connection closes.
     Settings panel opens its own stream.
5. **Network drops** -- browser's `EventSource` reconnects automatically with
   `Last-Event-ID`. Lifeline replays control events. Panel stream replays
   missed data updates.
6. **Lifeline stays warm** -- even while panel streams churn, the app shell
   never loses awareness of notifications or nav state.

> **Tip:** Use `SetBundleOnReconnect` on high-volume panel topics to reduce DOM
> churn when replaying many missed messages after a reconnect.

> **Tip:** The lifeline connection should subscribe only to low-volume topics.
> If a panel topic produces hundreds of messages per second, use Approach B
> with separate connections -- this gives independent buffer and backpressure
> behavior.

---

## 28. Browser-safe rendering for high-frequency SSE

SSE delivery can be faster than the browser should repaint. Tavern's
backpressure, throttling, and adaptive degradation protect the **transport
layer** -- they keep subscriber buffers healthy and prevent slow clients from
stalling publishers. But once a message reaches the browser, the **render
layer** is your application's responsibility.

These are different concerns at different layers:

| Layer | Concern | Tools |
|-------|---------|-------|
| **Transport** | Buffer depth, drop policy, subscriber eviction | Tavern backpressure, `WithAdaptiveBackpressure`, `PublishBlocking` |
| **Render** | DOM mutation rate, layout thrash, paint budget | Application JavaScript, CSS containment, request-animation-frame discipline |

A stream publishing 50 messages/second through a healthy broker can still make
a page unusable if every message triggers synchronous DOM work. The fixes live
in client-side rendering code, not in Tavern configuration.

### Recommendations

**Do not assume one DOM update per message on hot streams.** When a topic
publishes faster than ~15-20 Hz, coalesce incoming messages and render on a
bounded window. A 25-100 ms render interval is a reasonable starting point --
tune from there based on what the user actually needs to see.

**Prefer append + bounded logs over prepend + unbounded churn.** Appending to a
container is cheaper than prepending (no layout shift on existing elements).
Cap the container to a fixed number of children and remove overflow from the
tail. Unbounded growth eventually kills any page, no matter how well the
transport performs.

**Be cautious with unconditional auto-follow / auto-scroll.** Scrolling on
every message in a hot feed fights the user when they try to read. Gate
auto-scroll on whether the user is already at the bottom, and pause it when
they scroll up.

**Be cautious with View Transitions on hot live pages.** The View Transitions
API serializes transitions -- if a new SSE message triggers a transition while
the previous one is still animating, the browser queues or drops it. On a hot
page this causes visible stutter. Limit transitions to user-initiated
navigation or low-frequency updates, and skip them for high-cadence streams.

**A tiny client-side queue can be the right fix for hot views.** Rather than
fighting HTMX's eager swap behavior, intercept SSE events before they reach
the DOM, buffer them, and flush on a render tick. This decouples transport
cadence from render cadence without changing anything on the server.

### Example: coalesced render loop

A minimal pattern that buffers SSE messages and flushes at a bounded rate:

```html
<div id="feed" sse-connect="/sse/dashboard" sse-swap="update"></div>

<script>
(function () {
  const feed = document.getElementById("feed");
  const queue = [];
  const MAX_CHILDREN = 200;
  const RENDER_INTERVAL_MS = 50; // 20 Hz cap -- tune to taste

  // Intercept SSE messages before HTMX swaps them.
  feed.addEventListener("htmx:sseBeforeMessage", function (evt) {
    evt.preventDefault(); // stop HTMX from swapping immediately
    queue.push(evt.detail.elt);
  });

  // Flush the queue on a fixed interval.
  setInterval(function () {
    if (queue.length === 0) return;

    const fragment = document.createDocumentFragment();
    while (queue.length > 0) {
      fragment.appendChild(queue.shift());
    }
    feed.appendChild(fragment);

    // Trim overflow from the top.
    while (feed.children.length > MAX_CHILDREN) {
      feed.removeChild(feed.firstElementChild);
    }
  }, RENDER_INTERVAL_MS);
})();
</script>
```

> **Note:** The exact HTMX event name and `detail` shape depend on the
> `hx-ext="sse"` extension version. Check the
> [HTMX SSE extension docs](https://htmx.org/extensions/sse/) for the current
> API. The pattern -- intercept, buffer, flush on a timer -- is the same
> regardless of the event plumbing.

### When to reach for this

- Dashboard panels updating more than ~10 times/second
- Activity feeds or log tails on topics with burst traffic
- Any page where users report "it feels laggy" but Tavern metrics show healthy
  delivery and no drops

If transport metrics look fine but the page stutters, the bottleneck is almost
certainly in the render layer. Add a coalesced render window before reaching for
server-side throttling.

---

## 29. Interaction insulation for hot-region commands

When the DOM inside an SSE-swapped region is replaced faster than the user can
click, node-bound handlers (`hx-post`, `onclick`) break because the target node
is gone before the browser dispatches the event. Delegated commands solve this
by binding the listener on the stable `sse-connect` parent and matching
ephemeral children at event time.

### Go handler (topic publisher)

```go
func tickerPublisher(ctx context.Context, broker *tavern.SSEBroker, db *sql.DB) {
    ticker := time.NewTicker(500 * time.Millisecond)
    defer ticker.Stop()
    for {
        select {
        case <-ticker.C:
            tasks := fetchActiveTasks(db)
            broker.PublishOOB("task-feed",
                tavern.Replace("task-list", renderTaskList(tasks)),
            )
        case <-ctx.Done():
            return
        }
    }
}
```

### Go handler (command endpoint)

```go
func completeTask(broker *tavern.SSEBroker, db *sql.DB) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var body struct {
            ID string `json:"id"`
        }
        json.NewDecoder(r.Body).Decode(&body)
        markComplete(db, body.ID)

        // The updated task list arrives via SSE on the next tick.
        w.WriteHeader(http.StatusNoContent)
    }
}

mux.Handle("/sse/tasks", broker.SSEHandler("task-feed"))
mux.HandleFunc("POST /tasks/complete", completeTask(broker, db))
```

### HTML (tavern-js delegated commands)

```html
<script src="https://cdn.jsdelivr.net/gh/catgoose/tavern-js@latest/dist/tavern.min.js"></script>

<div id="task-region"
     sse-connect="/sse/tasks"
     sse-swap="message"
     tavern-command-delegate="pointerdown"
     tavern-command-target="[command-url]">

  <!-- Replaced by SSE every 500ms — buttons are ephemeral -->
  <div id="task-list">
    <button command-url="/tasks/complete" command-id="42">Done</button>
    <button command-url="/tasks/complete" command-id="43">Done</button>
  </div>
</div>
```

### How it works

1. `tavern-command-delegate="pointerdown"` binds a single listener on the
   `sse-connect` element for `pointerdown` events.
2. When the user clicks a button, `closest("[command-url]")` finds the nearest
   matching element -- even though that element may be replaced milliseconds
   later.
3. `command-url` becomes the POST endpoint. All other `command-*` attributes
   become the JSON body (`command-id="42"` becomes `{"id": "42"}`).
4. `Tavern.command(url, body)` fires automatically. The server processes the
   command and publishes the result back over SSE.

> **Why `pointerdown` instead of `click`?** In very hot regions, a `click`
> (which fires on button release) races with the next SSE swap. `pointerdown`
> captures intent at first contact, before the DOM can churn.

> Normal forms and `hx-post` remain the right choice outside hotspots.
> Delegated commands are an escape hatch for regions where SSE replacement
> outpaces human interaction speed.

---

## 30. Consuming structured control events in app code

Tavern's control events carry structured JSON payloads. While
[tavern-js](https://github.com/catgoose/tavern-js) handles common UX
patterns declaratively, your app code can listen for the same DOM events
to implement custom recovery logic.

For the full control event reference and delivery scenarios, see
[Delivery Observability](docs/delivery-observability.md).

### Listen for control events

```html
<div id="dashboard"
     sse-connect="/sse/dashboard"
     sse-swap="update"
     tavern-stale-class="opacity-50"
     tavern-live-class="opacity-100">

  <span tavern-status-live>Live</span>
  <span tavern-status-stale class="hidden">Data may be stale</span>
  <span tavern-status-recovering class="hidden">Reconnecting...</span>
</div>

<script>
const dash = document.getElementById("dashboard");

// tavern:reconnected — check recovery quality
dash.addEventListener("tavern:reconnected", () => {
  // tavern-js restores the live state automatically.
  // Use this hook for app-level side effects.
  console.log("Dashboard reconnected");
});

// tavern:replay-gap — the replay log couldn't satisfy Last-Event-ID.
// The region is stale until fresh data arrives.
dash.addEventListener("tavern:stale", (e) => {
  if (e.detail.reason === "replay-gap") {
    showToast("Dashboard data may be incomplete. Refreshing...");
    htmx.ajax("GET", "/partials/dashboard", { target: "#dashboard" });
  }
});

// tavern:live — region is fully caught up again
dash.addEventListener("tavern:live", () => {
  clearToast();
});
</script>
```

### Listening for raw SSE control events (without tavern-js)

If you use a raw `EventSource` without tavern-js, listen directly on the SSE
event types to access the full structured payloads:

```js
const es = new EventSource("/sse/dashboard");

es.addEventListener("tavern-reconnected", (e) => {
  const { replayDelivered, replayDropped } = JSON.parse(e.data);
  if (replayDropped > 0) {
    showBanner(`Reconnected, but ${replayDropped} messages were lost`);
  } else {
    console.log(`Clean recovery: ${replayDelivered} messages replayed`);
  }
});

es.addEventListener("tavern-replay-gap", (e) => {
  const { lastEventId } = JSON.parse(e.data);
  console.warn("Gap detected — last known ID:", lastEventId);
  // Trigger a full page refresh or re-fetch the affected region.
  location.reload();
});

es.addEventListener("tavern-replay-truncated", (e) => {
  const { delivered, dropped } = JSON.parse(e.data);
  showBanner(`Partial recovery: ${delivered} replayed, ${dropped} lost`);
});

es.addEventListener("tavern-topics-changed", (e) => {
  const { action, topic, topics } = JSON.parse(e.data);
  console.log(`Topic ${action}: ${topic}. Active topics:`, topics);
});
```

> **tavern-js vs raw SSE:** tavern-js translates SSE control events into
> declarative class changes, status elements, and DOM events -- no custom
> JavaScript needed for common patterns. Use the raw `EventSource` approach
> only when you need full control over recovery logic.

---

## 31. App-shell with lifeline and scoped streams

> **Contract reference:** See [docs/stream-contract.md](docs/stream-contract.md)
> for the full stream contract and decision guidance table.

A complete app-shell pattern with a persistent lifeline for control-plane
events and scoped connections for high-bandwidth panel data. This recipe
extends [Recipe 27](#recipe-27-app-shell-lifeline-with-scoped-panel-streams)
with dynamic topic management, navigation handling, and client-side
`tavern:topics-changed` integration.

### Go server setup

```go
broker := tavern.NewSSEBroker(
    tavern.WithBufferSize(32),
    tavern.WithKeepalive(15*time.Second),
)

// Replay for panel topics.
broker.SetReplayPolicy("dashboard-data", 50)
broker.SetReplayGapPolicy("dashboard-data", tavern.GapFallbackToSnapshot, func() string {
    return renderDashboard()
})
```

### Lifeline handler (control + notifications)

```go
func lifelineHandler(broker *tavern.SSEBroker) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        sessionID := r.Header.Get("X-Session-ID")

        ch, unsub := broker.SubscribeMultiWithMeta(
            tavern.SubscribeMeta{ID: sessionID},
            "notifications", "nav-state",
        )
        defer unsub()

        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")
        flusher, _ := w.(http.Flusher)

        for {
            select {
            case msg, ok := <-ch:
                if !ok {
                    return
                }
                fmt.Fprintf(w, "event: %s\ndata: %s\n\n", msg.Topic, msg.Data)
                flusher.Flush()
            case <-r.Context().Done():
                return
            }
        }
    }
}

mux.Handle("/sse/app", lifelineHandler(broker))
```

### Navigation handler (AddTopic / RemoveTopic)

```go
func navigateHandler(broker *tavern.SSEBroker) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        sessionID := r.Header.Get("X-Session-ID")
        panel := r.FormValue("panel")
        action := r.FormValue("action") // "open" or "close"

        topic := panel + "-data"
        switch action {
        case "open":
            broker.AddTopic(sessionID, topic, true) // sends tavern-topics-changed
        case "close":
            broker.RemoveTopic(sessionID, topic, true)
        }
        w.WriteHeader(http.StatusNoContent)
    }
}

mux.HandleFunc("POST /navigate", navigateHandler(broker))
```

### Scoped panel handler (high-bandwidth detail view)

```go
// Independent SSE connection for high-frequency data.
mux.Handle("/sse/panel/analytics", broker.SSEHandler("analytics-data",
    tavern.WithMaxConnectionDuration(10*time.Minute),
))
```

### HTML (lifeline + scoped connections)

```html
<script src="https://cdn.jsdelivr.net/gh/catgoose/tavern-js@latest/dist/tavern.min.js"></script>

<!-- Lifeline: persistent app-shell connection -->
<div id="app-shell"
     sse-connect="/sse/app"
     tavern-reconnecting-class="opacity-50">

  <nav id="nav-state" sse-swap="nav-state"></nav>
  <div id="notifications" sse-swap="notifications"></div>
  <div tavern-status class="hidden">Reconnecting app shell...</div>

  <!-- Panel region: swap target created when topic is added -->
  <main id="panel-content"></main>
</div>

<!-- Scoped panel: separate connection, independent backpressure -->
<template id="analytics-panel-template">
  <div sse-connect="/sse/panel/analytics"
       sse-swap="analytics-data"
       tavern-stale-class="opacity-50"
       tavern-live-class="opacity-100">
    <div id="analytics-chart"></div>
    <span tavern-status-stale class="hidden">Analytics data stale</span>
  </div>
</template>
```

### Client-side topic change handling

```js
document.getElementById("app-shell").addEventListener("tavern:topics-changed", (e) => {
  const { action, topic, topics } = e.detail;

  if (action === "added") {
    // Create a swap target for the new topic.
    const target = document.createElement("div");
    target.id = topic + "-region";
    target.setAttribute("sse-swap", topic);
    document.getElementById("panel-content").appendChild(target);
  }

  if (action === "removed") {
    const el = document.getElementById(topic + "-region");
    if (el) el.remove();
  }

  console.log("Active topics:", topics);
});

// Navigation: open/close panels via AddTopic/RemoveTopic
function openPanel(panel) {
  fetch("/navigate", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: `panel=${panel}&action=open`,
  });
}

function closePanel(panel) {
  fetch("/navigate", {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: `panel=${panel}&action=close`,
  });
}
```

### What happens

1. **Page loads** -- browser opens `/sse/app`. Lifeline delivers notifications
   and nav-state.
2. **User opens a panel** -- `openPanel("dashboard")` calls `AddTopic`. The
   lifeline starts delivering `dashboard-data` events. A
   `tavern-topics-changed` event fires and the client creates a swap target.
3. **User opens analytics** -- a separate `sse-connect` opens
   `/sse/panel/analytics` with its own buffer and backpressure.
4. **Network drops** -- each stream reconnects independently. Lifeline replays
   control events; panel stream replays data. No cross-contamination.
5. **User closes dashboard** -- `closePanel("dashboard")` calls `RemoveTopic`.
   The client removes the swap target.

> **When to use AddTopic vs scoped connection:** Low-bandwidth topics that
> share the lifeline's lifecycle belong on `AddTopic`. High-bandwidth topics
> that need independent buffer sizing or per-user scoping belong on a separate
> scoped connection. See [docs/stream-contract.md](docs/stream-contract.md)
> for the full decision guidance.

---

## 32. Stale/degraded/live region handling

The full lifecycle of a Tavern-connected region, from initial connection
through disconnection, recovery, and potential staleness. This recipe shows
how to wire up visual indicators and programmatic hooks for every state.

### State machine

```
connecting → LIVE → (sseError) → DISCONNECTED → (sseOpen) → RECOVERING
RECOVERING → (tavern-reconnected) → LIVE
RECOVERING → (tavern-replay-gap) → STALE
STALE → (tavern-reconnected with clean replay) → LIVE
LIVE → (replay-gap without reload) → STALE
```

### HTML (visual state indicators)

```html
<script src="https://cdn.jsdelivr.net/gh/catgoose/tavern-js@latest/dist/tavern.min.js"></script>

<div id="feed"
     sse-connect="/sse/feed"
     sse-swap="post"
     tavern-stale-class="border-amber-500 bg-amber-50"
     tavern-live-class="border-green-500 bg-white"
     tavern-reconnecting-class="opacity-50 pointer-events-none"
     class="border-2 rounded-lg p-4 transition-all duration-300">

  <!-- State-specific status elements (tavern-js shows/hides automatically) -->
  <div tavern-status-live class="hidden text-green-600 text-sm mb-2">
    Live
  </div>
  <div tavern-status-stale class="hidden text-amber-600 text-sm mb-2">
    Data may be incomplete — waiting for recovery
  </div>
  <div tavern-status-recovering class="hidden text-blue-600 text-sm mb-2">
    <span class="animate-pulse">Reconnecting...</span>
  </div>

  <!-- Normal SSE content -->
  <div id="feed-content"></div>
</div>
```

### CSS

```css
/* Base transition for smooth state changes */
#feed {
  transition: border-color 0.3s, background-color 0.3s, opacity 0.3s;
}

/* tavern-js applies these classes automatically */
.border-amber-500 { border-color: #f59e0b; }
.bg-amber-50 { background-color: #fffbeb; }
.border-green-500 { border-color: #22c55e; }
.bg-white { background-color: #fff; }
.opacity-50 { opacity: 0.5; }
.pointer-events-none { pointer-events: none; }
```

### Go server (replay + gap fallback)

```go
broker.SetReplayPolicy("feed", 100)
broker.SetReplayGapPolicy("feed", tavern.GapFallbackToSnapshot, func() string {
    return renderFeed()
})
broker.SetBundleOnReconnect("feed", true)

mux.Handle("/sse/feed", broker.SSEHandler("feed"))
```

### How the states interact

**LIVE:** The region has an active SSE connection and is receiving fresh data.
`tavern-live-class` is applied, `tavern-status-live` is visible. No action
needed.

**DISCONNECTED:** The SSE connection dropped (`sseError` fired).
`tavern-reconnecting-class` is applied, `tavern-status-recovering` becomes
visible. The browser's `EventSource` auto-reconnects. Interactive elements
are dimmed and non-clickable.

**RECOVERING:** The transport is open again (`sseOpen`) but the server hasn't
confirmed recovery yet. The region stays in the reconnecting visual state
until `tavern-reconnected` arrives.

**LIVE (clean recovery):** The server replayed all missed messages and sent
`tavern-reconnected` with `replayDropped: 0`. tavern-js removes reconnecting
classes, applies `tavern-live-class`, and shows `tavern-status-live`. The
region is fully caught up.

**STALE (gap or truncation):** The server sent `tavern-replay-gap` (ID not in
replay log) or `tavern-replay-truncated` (buffer too small). tavern-js applies
`tavern-stale-class` and shows `tavern-status-stale`. The region has missed
messages and may show incomplete data.

**STALE to LIVE:** When fresh data eventually brings the region up to date
(either through a gap fallback snapshot or normal publishes), the region
transitions back to live. A subsequent `tavern-reconnected` with clean
replay also clears stale state.

### Programmatic hooks for custom recovery

```js
const feed = document.getElementById("feed");

feed.addEventListener("tavern:stale", (e) => {
  // e.detail.reason tells you why: "replay-gap" or "replay-truncated"
  if (e.detail.reason === "replay-gap") {
    // Gap fallback snapshot may have already been delivered by the server.
    // Show a manual refresh button as a safety net.
    showRefreshButton();
  }
});

feed.addEventListener("tavern:live", () => {
  hideRefreshButton();
});

feed.addEventListener("tavern:recovering", () => {
  // Optimistic: disable interactions while recovery is in progress
  disableFormInputs();
});

feed.addEventListener("tavern:reconnected", () => {
  enableFormInputs();
});
```

> **Gap vs truncation:** A gap means the `Last-Event-ID` wasn't in the
> replay log at all -- unknown message loss. A truncation means the server
> found the ID but the subscriber's buffer couldn't hold all replayed
> messages -- known, bounded loss. Both result in stale state, but
> truncation gives you exact `delivered`/`dropped` counts for the UI.

> **Server-side configuration matters:** `SetReplayGapPolicy` with
> `GapFallbackToSnapshot` delivers a fresh snapshot on gap, which often
> brings the region back to a usable state immediately. Without a fallback
> policy, the region stays stale until normal publishes refresh it.
