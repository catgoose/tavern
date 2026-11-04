package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// topicBoard carries every invalidation. Each message is an SSE event named
// after the region that changed (column-todo, column-doing, column-done,
// activity) with the board version as data. Clients refetch the region from
// the server instead of trusting message contents. One topic keeps all
// events, and the shutdown retry, in order on the connection.
const topicBoard = "board"

//go:embed static
var static embed.FS

// Config holds the stream settings recommended by the production guide.
type Config struct {
	Buffer         int           // queued invalidations per connection
	Heartbeat      time.Duration // keeps proxies from closing idle streams
	WriteTimeout   time.Duration // bounds each frame write
	MaxSubscribers int           // admission limit; 0 = unlimited
	RetryOnClose   time.Duration // reconnect delay sent at shutdown
}

func DefaultConfig() Config {
	return Config{
		Buffer:         16,
		Heartbeat:      15 * time.Second,
		WriteTimeout:   10 * time.Second,
		MaxSubscribers: 1000,
		RetryOnClose:   3 * time.Second,
	}
}

// App is the task board: canonical state, the broker and the HTTP routes.
type App struct {
	cfg    Config
	board  *Board
	broker *tavern.SSEBroker
	logger *slog.Logger
	mux    *http.ServeMux

	done    chan struct{} // closed by Shutdown
	pausers sync.Map      // connection id → *pauser (demo slow connection)
	counter counterScenario

	collab        *collab
	collabStreams collabStreams
}

func NewApp(cfg Config, logger *slog.Logger) *App {
	opts := []tavern.BrokerOption{tavern.WithBufferSize(cfg.Buffer), tavern.WithLogger(logger)}
	if cfg.MaxSubscribers > 0 {
		opts = append(opts, tavern.WithMaxSubscribers(cfg.MaxSubscribers))
	}
	a := &App{
		cfg:    cfg,
		board:  NewBoard(),
		broker: tavern.NewSSEBroker(opts...),
		logger: logger,
		mux:    http.NewServeMux(),
		done:   make(chan struct{}),
	}
	a.mux.HandleFunc("GET /{$}", a.page)
	a.mux.Handle("GET /static/", http.FileServerFS(static))
	a.mux.HandleFunc("GET /events", a.events)
	a.mux.HandleFunc("GET /columns/{column}", a.column)
	a.mux.HandleFunc("GET /activity", a.activity)
	a.mux.HandleFunc("POST /tasks", a.addTask)
	a.mux.HandleFunc("POST /tasks/{id}/move", a.moveTask)
	a.mux.HandleFunc("POST /demo/pause", a.pause)
	a.mux.HandleFunc("POST /demo/resume", a.resume)
	a.mux.HandleFunc("POST /demo/sample", a.sample)
	a.registerShowcase()
	a.registerCollab()
	return a
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// Shutdown asks browsers to wait before reconnecting, then ends every stream.
// Call it before http.Server.Shutdown, which does not end streams itself.
func (a *App) Shutdown() {
	a.broker.SetRetryAll(a.cfg.RetryOnClose)
	close(a.done)
	a.collabStreams.holds.close()
	a.collab.feed.close()
	a.broker.Close()
}

// events streams invalidations for every region on one connection. The first
// frame is "ready", sent after the subscription exists; clients fetch the
// canonical board when they receive it, so no change can fall between the
// fetch and the subscription. If this connection cannot keep up, the stream
// ends instead of silently dropping an invalidation, and the browser
// reconnects and fetches again.
func (a *App) events(w http.ResponseWriter, r *http.Request) {
	status := tavern.NewSubscriptionStatus()
	ch, unsub := a.broker.SubscribeWith(topicBoard,
		tavern.SubWithDisconnectOnLoss(), tavern.SubWithStatus(status))
	if ch == nil {
		http.Error(w, "Too many connections, try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer unsub()

	conn := r.URL.Query().Get("conn")
	gate := a.pauserFor(conn)
	defer a.pausers.CompareAndDelete(conn, gate)
	encode := func(msg string) string {
		gate.wait(r.Context(), a.done)
		return msg
	}
	err := tavern.StreamSSE(r.Context(), w, ch, encode,
		tavern.WithStreamSnapshot(func() string {
			return tavern.NewSSEMessage("ready", strconv.FormatUint(a.board.Version(), 10)).String()
		}),
		tavern.WithStreamHeartbeat(a.cfg.Heartbeat),
		tavern.WithStreamWriteTimeout(a.cfg.WriteTimeout),
		tavern.WithStreamWriteErrors(),
	)
	// A client that left ends as an unsubscribe; an earlier cause such as
	// queue loss or shutdown is kept.
	unsub()
	a.logger.Info("stream ended", "reason", status.Termination().Reason.String(), "write_error", err)
}

// invalidate tells every connection that regions changed.
func (a *App) invalidate(version uint64, cols ...string) {
	v := strconv.FormatUint(version, 10)
	for _, c := range cols {
		a.broker.Publish(topicBoard, tavern.NewSSEMessage("column-"+c, v).String())
	}
	a.broker.Publish(topicBoard, tavern.NewSSEMessage("activity", v).String())
}

func (a *App) addTask(w http.ResponseWriter, r *http.Request) {
	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" || len(title) > 120 {
		http.Error(w, "Give the task a title of up to 120 characters", http.StatusBadRequest)
		return
	}
	_, version := a.board.Add(title, viewerName(r))
	a.invalidate(version, "todo")
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) moveTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	to := r.FormValue("to")
	if err != nil || !validColumn(to) {
		http.Error(w, "Unknown task or column", http.StatusBadRequest)
		return
	}
	from, version, ok := a.board.Move(id, to, viewerName(r))
	if !ok {
		http.Error(w, "That task no longer exists", http.StatusNotFound)
		return
	}
	a.invalidate(version, from, to)
	w.WriteHeader(http.StatusNoContent)
}

// sample adds several tasks at once, which is enough to overflow a paused
// connection's queue.
func (a *App) sample(w http.ResponseWriter, r *http.Request) {
	for i := 1; i <= 10; i++ {
		_, version := a.board.Add("Sample task "+strconv.Itoa(i), viewerName(r))
		a.invalidate(version, "todo")
	}
	w.WriteHeader(http.StatusNoContent)
}

// viewerName is a display name chosen on the page. It is not authentication:
// a real application derives identity from its session.
func viewerName(r *http.Request) string {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 40 {
		return "Someone"
	}
	return name
}

func newConnID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// pauser holds back one connection's writes to simulate a slow client.
type pauser struct {
	mu     sync.Mutex
	paused bool
	resume chan struct{}
}

func (a *App) pauserFor(conn string) *pauser {
	p, _ := a.pausers.LoadOrStore(conn, &pauser{resume: make(chan struct{})})
	return p.(*pauser)
}

func (p *pauser) set(paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if paused == p.paused {
		return
	}
	p.paused = paused
	if !paused {
		close(p.resume)
		p.resume = make(chan struct{})
	}
}

// wait blocks while paused, until resumed, the request ends or the app shuts
// down.
func (p *pauser) wait(ctx context.Context, done <-chan struct{}) {
	p.mu.Lock()
	paused, resume := p.paused, p.resume
	p.mu.Unlock()
	if !paused {
		return
	}
	select {
	case <-resume:
	case <-ctx.Done():
	case <-done:
	}
}

func (a *App) pause(w http.ResponseWriter, r *http.Request)  { a.setPaused(w, r, true) }
func (a *App) resume(w http.ResponseWriter, r *http.Request) { a.setPaused(w, r, false) }

func (a *App) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	v, ok := a.pausers.Load(r.URL.Query().Get("conn"))
	if !ok {
		http.Error(w, "This tab has no live connection", http.StatusNotFound)
		return
	}
	v.(*pauser).set(paused)
	w.WriteHeader(http.StatusNoContent)
}

// Rendering.

var pageTmpl = template.Must(template.New("page").Parse(pageHTML))

var fragments = template.Must(template.New("").Parse(fragmentsHTML))

func (a *App) page(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = "Viewer " + newConnID()[:4]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := pageTmpl.Execute(w, map[string]any{
		"Conn":    newConnID(),
		"Name":    name,
		"Columns": columns,
		"Titles":  columnTitles,
		"Buffer":  a.cfg.Buffer,
	})
	if err != nil {
		a.logger.Error("render page", "err", err)
	}
}

func (a *App) column(w http.ResponseWriter, r *http.Request) {
	c := r.PathValue("column")
	if !validColumn(c) {
		http.NotFound(w, r)
		return
	}
	a.render(w, "column", map[string]any{
		"Column": c, "Columns": columns, "Titles": columnTitles, "Tasks": a.board.Column(c),
	})
}

func (a *App) activity(w http.ResponseWriter, _ *http.Request) {
	a.render(w, "activity", a.board.Activity())
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := fragments.ExecuteTemplate(w, name, data); err != nil {
		a.logger.Error("render fragment", "name", name, "err", err)
	}
}
