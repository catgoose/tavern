package main

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// The collaboration scenario keeps its own board so the reference board at /
// keeps its invalidation contract.
//
// Delivery:
//   - topicCollab carries each persona's board changes and inbox notices,
//     published to that persona's scope. A new
//     stream starts from a snapshot taken while the board is locked, so no
//     change falls between the snapshot and the subscription.
//   - Each role has an activity topic and a stats topic, published with
//     PublishWithReplay as whole representations and served together by a
//     DynamicGroup, so a new feed stream starts from the latest of each. The
//     group's stream is not loss-aware, so a dropped feed update is repaired
//     by republishing the current feed (see feedRepair).
const (
	topicCollab = "collab/board"
	groupFeed   = "collab-feed"

	collabActivityLimit = 12
	collabInboxLimit    = 8
	collabTitleMax      = 120
)

func activityTopic(r *role) string { return "collab/activity/" + r.name }
func statsTopic(r *role) string    { return "collab/stats/" + r.name }

// role decides what a persona may see and do.
type role struct {
	name         string
	label        string
	seesInternal bool
}

var (
	roleLead        = &role{name: "lead", label: "Team lead", seesInternal: true}
	roleContributor = &role{name: "contributor", label: "Contributor"}
)

// persona is a fixed demo identity. The selector on the page is a demo switch,
// not a login: a real application takes identity from its session.
type persona struct {
	id   string
	name string
	role *role
	// notifies lists the inbox notice kinds this persona receives. The broker
	// filter on the persona's stream and the inbox snapshot both use it.
	notifies map[string]bool
}

const (
	noticeAssigned  = "assigned"
	noticeCompleted = "completed"
)

var personas = []*persona{
	{id: "alice", name: "Alice", role: roleLead, notifies: map[string]bool{noticeAssigned: true, noticeCompleted: true}},
	{id: "bob", name: "Bob", role: roleContributor, notifies: map[string]bool{noticeAssigned: true}},
}

var roles = []*role{roleLead, roleContributor}

func (p *persona) ID() string   { return p.id }
func (p *persona) Name() string { return p.name }
func (p *persona) Role() *role  { return p.role }

func (r *role) Label() string      { return r.label }
func (r *role) SeesInternal() bool { return r.seesInternal }

func personaByID(id string) (*persona, bool) {
	for _, p := range personas {
		if p.id == id {
			return p, true
		}
	}
	return nil, false
}

func (p *persona) sees(t *collabTask) bool { return p.role.sees(t) }

func (r *role) sees(t *collabTask) bool { return r.allows(t.Internal) }

// allows reports whether the role may see something marked internal.
func (r *role) allows(internal bool) bool { return !internal || r.seesInternal }

type collabTask struct {
	ID       int
	Title    string
	Column   string
	Internal bool
	Creator  *persona
	Assignee *persona // nil when unassigned
}

// activityEntry is one line of the feed. Leads see Text; contributors see
// PublicText, and nothing when it is empty.
type activityEntry struct {
	Text       string
	PublicText string
}

func (e activityEntry) textFor(r *role) string {
	if r.seesInternal {
		return e.Text
	}
	return e.PublicText
}

type notice struct {
	ID   int
	Kind string
	Text string
}

// collab is the scenario's canonical state. mu orders every change with its
// publishes and with stream setup; it is never held while writing to a
// network connection.
type collab struct {
	mu       sync.Mutex
	broker   *tavern.SSEBroker
	nextID   int
	tasks    map[int]*collabTask
	activity []activityEntry     // newest first, bounded
	inbox    map[string][]notice // persona id → notices it receives, newest first, bounded
	nextNote int

	feed feedRepair

	// beforeSnapshot, when set by tests, runs inside the snapshot while mu
	// is held.
	beforeSnapshot func()
}

// Feed repair delays: the first republish waits feedRepairBase, and each
// republish that is itself dropped doubles the wait, up to feedRepairMax.
const (
	feedRepairBase = 200 * time.Millisecond
	feedRepairMax  = 2 * time.Second
)

// feedRepair republishes the feed after a feed update is dropped. A drop
// means some feed stream's queue was full, so its page may keep an old
// activity list or counts with no later change to correct them. Republishing
// the current feed replaces them once that stream drains; earlier queued
// updates arrive first, so the newest state lands last. At most one
// republish is pending, and they stop once one goes out without a drop.
type feedRepair struct {
	mu        sync.Mutex
	base, max time.Duration
	delay     time.Duration
	timer     *time.Timer
	repairing bool // a republish is in progress
	dropped   bool // a feed update was dropped during it
	closed    bool
	republish func()
}

func (f *feedRepair) init(republish func()) {
	f.base, f.max, f.delay, f.republish = feedRepairBase, feedRepairMax, feedRepairBase, republish
}

// onDrop is the broker's publish-drop callback. It runs in the publishing
// goroutine, which holds collab.mu, so it only schedules.
func (f *feedRepair) onDrop(topic string) {
	if !strings.HasPrefix(topic, "collab/activity/") && !strings.HasPrefix(topic, "collab/stats/") {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.repairing {
		f.dropped = true
		return
	}
	f.scheduleLocked()
}

func (f *feedRepair) scheduleLocked() {
	if f.closed || f.timer != nil {
		return
	}
	f.timer = time.AfterFunc(f.delay, f.run)
}

func (f *feedRepair) run() {
	f.mu.Lock()
	f.timer = nil
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.repairing, f.dropped = true, false
	f.mu.Unlock()

	f.republish()

	f.mu.Lock()
	defer f.mu.Unlock()
	f.repairing = false
	if !f.dropped {
		f.delay = f.base
		return
	}
	f.delay = min(f.delay*2, f.max)
	f.scheduleLocked()
}

// pending reports whether a republish is scheduled.
func (f *feedRepair) pending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.timer != nil
}

func (f *feedRepair) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	if f.timer != nil {
		f.timer.Stop()
		f.timer = nil
	}
}

var (
	errNotFound  = errors.New("no such task")
	errForbidden = errors.New("not allowed")
	errInvalid   = errors.New("invalid input")
)

func newCollab(b *tavern.SSEBroker) *collab {
	c := &collab{broker: b, nextID: 1, tasks: map[int]*collabTask{}, inbox: map[string][]notice{}}
	c.feed.init(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.publishFeedLocked()
	})
	// The broker supports one drop callback; this app uses it only here.
	b.OnPublishDrop(func(topic string, _ int) { c.feed.onDrop(topic) })
	alice, _ := personaByID("alice")
	bob, _ := personaByID("bob")
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, seed := range []struct {
		title    string
		column   string
		internal bool
		creator  *persona
		assignee *persona
	}{
		{"Write the release notes", "todo", false, alice, bob},
		{"Review the upgrade guide", "doing", false, bob, nil},
		{"Plan the reorg (leads only)", "todo", true, alice, nil},
		{"Fix the login redirect", "done", false, bob, bob},
	} {
		t := &collabTask{ID: c.nextID, Title: seed.title, Column: seed.column, Internal: seed.internal, Creator: seed.creator, Assignee: seed.assignee}
		c.nextID++
		c.tasks[t.ID] = t
	}
	c.publishFeedLocked()
	return c
}

// visibleTask returns a task the persona may see.
func (c *collab) visibleTask(p *persona, id int) (*collabTask, error) {
	t, ok := c.tasks[id]
	if !ok || !p.sees(t) {
		return nil, errNotFound
	}
	return t, nil
}

func (c *collab) title(p *persona, id int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.visibleTask(p, id)
	if err != nil {
		return "", err
	}
	return t.Title, nil
}

func validTitle(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len([]rune(s)) > collabTitleMax {
		return "", fmt.Errorf("%w: give the task a title of up to %d characters", errInvalid, collabTitleMax)
	}
	return s, nil
}

func (c *collab) create(p *persona, title string, internal bool) error {
	title, err := validTitle(title)
	if err != nil {
		return err
	}
	if internal && !p.role.seesInternal {
		return fmt.Errorf("%w: only leads can create internal tasks", errForbidden)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &collabTask{ID: c.nextID, Title: title, Column: "todo", Internal: internal, Creator: p}
	c.nextID++
	c.tasks[t.ID] = t
	c.publishCardLocked(t, func(t *collabTask) []tavern.Fragment {
		return []tavern.Fragment{tavern.AppendComponent(columnID(t.Column), card{t})}
	})
	c.recordTaskLocked(t, fmt.Sprintf("%s added %q", p.name, t.Title))
	return nil
}

func (c *collab) rename(p *persona, id int, title string) error {
	title, err := validTitle(title)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.visibleTask(p, id)
	if err != nil {
		return err
	}
	old := t.Title
	t.Title = title
	c.publishCardLocked(t, func(t *collabTask) []tavern.Fragment {
		return []tavern.Fragment{tavern.ReplaceComponent(cardID(t.ID), cardBody{t})}
	})
	c.recordTaskLocked(t, fmt.Sprintf("%s renamed %q to %q", p.name, old, t.Title))
	return nil
}

func (c *collab) move(p *persona, id int, to string) error {
	if !validColumn(to) {
		return fmt.Errorf("%w: unknown column", errInvalid)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.visibleTask(p, id)
	if err != nil {
		return err
	}
	if t.Column == to {
		return nil
	}
	t.Column = to
	c.publishCardLocked(t, func(t *collabTask) []tavern.Fragment {
		return []tavern.Fragment{tavern.Delete(cardID(t.ID)), tavern.AppendComponent(columnID(t.Column), card{t})}
	})
	c.recordTaskLocked(t, fmt.Sprintf("%s moved %q to %s", p.name, t.Title, columnTitles[to]))
	if to == "done" {
		c.notifyLocked(t.Creator, noticeCompleted, fmt.Sprintf("%s finished %q, which you created", p.name, t.Title))
	}
	return nil
}

func (c *collab) assign(p *persona, id int, to *persona) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.visibleTask(p, id)
	if err != nil {
		return err
	}
	if to != nil && !to.sees(t) {
		return fmt.Errorf("%w: %s cannot see this task", errForbidden, to.name)
	}
	t.Assignee = to
	c.publishCardLocked(t, func(t *collabTask) []tavern.Fragment {
		return []tavern.Fragment{tavern.ReplaceComponent(cardID(t.ID), cardBody{t})}
	})
	if to == nil {
		c.recordTaskLocked(t, fmt.Sprintf("%s unassigned %q", p.name, t.Title))
		return nil
	}
	c.recordTaskLocked(t, fmt.Sprintf("%s assigned %q to %s", p.name, t.Title, to.name))
	c.notifyLocked(to, noticeAssigned, fmt.Sprintf("%s assigned %q to you", p.name, t.Title))
	return nil
}

func (c *collab) remove(p *persona, id int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, err := c.visibleTask(p, id)
	if err != nil {
		return err
	}
	delete(c.tasks, id)
	c.publishCardLocked(t, func(t *collabTask) []tavern.Fragment {
		return []tavern.Fragment{tavern.Delete(cardID(t.ID))}
	})
	c.recordTaskLocked(t, fmt.Sprintf("%s deleted %q", p.name, t.Title))
	return nil
}

// clearDone deletes every finished task the persona can see. Each viewer's
// removals go out as one batch: Flush concatenates them into a single write
// per topic and scope. It is not a transaction, and the feed updates are
// separate publishes.
func (c *collab) clearDone(p *persona) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	batch := c.broker.Batch()
	all, public := 0, 0
	for _, t := range c.sortedLocked() {
		if t.Column != "done" || !p.sees(t) {
			continue
		}
		delete(c.tasks, t.ID)
		all++
		if !t.Internal {
			public++
		}
		for _, viewer := range personas {
			if viewer.sees(t) {
				batch.PublishOOBTo(topicCollab, viewer.id, tavern.Delete(cardID(t.ID)))
			}
		}
	}
	batch.Flush()
	if all == 0 {
		return 0
	}
	// Contributors are told only about the tasks they could see.
	entry := activityEntry{Text: fmt.Sprintf("%s cleared %s", p.name, finished(all))}
	if public > 0 {
		entry.PublicText = fmt.Sprintf("%s cleared %s", p.name, finished(public))
	}
	c.recordLocked(entry)
	return all
}

func finished(n int) string {
	if n == 1 {
		return "1 finished task"
	}
	return fmt.Sprintf("%d finished tasks", n)
}

// publishCardLocked sends a card change to every persona who may see the
// task. Rendering is skipped when the topic has no subscribers.
func (c *collab) publishCardLocked(t *collabTask, frags func(*collabTask) []tavern.Fragment) {
	for _, viewer := range personas {
		if !viewer.sees(t) {
			continue
		}
		c.broker.PublishLazyOOBTo(topicCollab, viewer.id, func() []tavern.Fragment { return frags(t) })
	}
}

// recordTaskLocked records an entry about one task, shown to the roles that
// may see the task.
func (c *collab) recordTaskLocked(t *collabTask, text string) {
	entry := activityEntry{Text: text}
	if !t.Internal {
		entry.PublicText = text
	}
	c.recordLocked(entry)
}

// recordLocked adds an activity entry and republishes every role's feed.
func (c *collab) recordLocked(entry activityEntry) {
	stamp := time.Now().Format("15:04:05") + "  "
	entry.Text = stamp + entry.Text
	if entry.PublicText != "" {
		entry.PublicText = stamp + entry.PublicText
	}
	c.activity = append([]activityEntry{entry}, c.activity...)
	if len(c.activity) > collabActivityLimit {
		c.activity = c.activity[:collabActivityLimit]
	}
	c.publishFeedLocked()
}

// publishFeedLocked publishes each role's activity list and counts as whole
// representations. PublishWithReplay keeps the latest of each, so a new feed
// stream starts with them and a missed one is replaced by the next.
func (c *collab) publishFeedLocked() {
	for _, r := range roles {
		c.broker.PublishWithReplay(activityTopic(r), tavern.RenderFragments(tavern.Replace("collab-activity", c.renderActivityLocked(r))))
		c.broker.PublishWithReplay(statsTopic(r), tavern.RenderFragments(tavern.Replace("collab-stats", c.renderStatsLocked(r))))
	}
}

// notifyLocked sends p a notice. Like a notification service, it publishes
// every notice to the recipient's scope and leaves p's preferences to the
// filter on p's stream, so a kind p does not take never reaches p's browser.
// The inbox kept for p's snapshots holds only the kinds p takes, newest
// first and bounded; the oldest is removed from the page as well.
func (c *collab) notifyLocked(p *persona, kind, text string) {
	c.nextNote++
	n := notice{ID: c.nextNote, Kind: kind, Text: time.Now().Format("15:04:05") + "  " + text}
	frags := []tavern.Fragment{tavern.Prepend("collab-inbox", renderNotice(n))}
	if p.notifies[kind] {
		inbox := append([]notice{n}, c.inbox[p.id]...)
		if len(inbox) > collabInboxLimit {
			frags = append(frags, tavern.Delete(noticeID(inbox[collabInboxLimit].ID)))
			inbox = inbox[:collabInboxLimit]
		}
		c.inbox[p.id] = inbox
	}
	c.broker.PublishTo(topicCollab, p.id, tavern.NewSSEMessage(noticeEvent(kind), tavern.RenderFragments(frags...)).String())
}

func noticeEvent(kind string) string { return "inbox-" + kind }

// streamFilter returns the broker predicate for p's stream. Board changes
// are raw OOB HTML and always pass; notices are SSE frames whose event name
// carries their kind. The predicate reads only the app-generated first line.
func streamFilter(p *persona) tavern.FilterPredicate {
	return func(msg string) bool {
		kind, ok := strings.CutPrefix(msg, "event: inbox-")
		if !ok {
			return true
		}
		kind, _, _ = strings.Cut(kind, "\n")
		return p.notifies[kind]
	}
}

// subscribe opens p's stream. The snapshot is taken and the subscription
// registered while mu is held, so every change is either in the snapshot
// or delivered after it. Holding mu only covers queueing the snapshot, never
// a network write.
func (c *collab) subscribe(p *persona, status *tavern.SubscriptionStatus) (msgs <-chan string, unsubscribe func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.broker.SubscribeWith(topicCollab,
		tavern.SubWithScope(p.id),
		tavern.SubWithSnapshot(func() string { return c.snapshotLocked(p) }),
		tavern.SubWithFilter(streamFilter(p)),
		tavern.SubWithDisconnectOnLoss(),
		tavern.SubWithStatus(status),
	)
}

// snapshotLocked replaces every column and the inbox with p's current view.
func (c *collab) snapshotLocked(p *persona) string {
	if c.beforeSnapshot != nil {
		c.beforeSnapshot()
	}
	frags := make([]tavern.Fragment, 0, len(columns)+1)
	for _, col := range columns {
		frags = append(frags, tavern.Replace(columnID(col), c.renderColumnLocked(p.role, col)))
	}
	var inbox strings.Builder
	for _, n := range c.inbox[p.id] {
		inbox.WriteString(renderNotice(n))
	}
	frags = append(frags, tavern.Replace("collab-inbox", inbox.String()))
	return tavern.RenderFragments(frags...)
}

// feedTopics is the DynamicGroup function: the topics come from the
// persona's role, never from the request.
func feedTopics(p *persona) []string {
	return []string{activityTopic(p.role), statsTopic(p.role)}
}

// Rendering. User text is escaped by html/template.

func columnID(col string) string { return "collab-col-" + col }
func cardID(id int) string       { return fmt.Sprintf("collab-task-%d", id) }
func noticeID(id int) string     { return fmt.Sprintf("collab-notice-%d", id) }

// card renders a task with the element that OOB updates target, for
// appending to a column. cardBody renders what goes inside it: Replace
// swaps in a new element with the same ID around that content. Both
// implement tavern.Component.
type (
	card     struct{ t *collabTask }
	cardBody struct{ t *collabTask }
)

func (c card) Render(_ context.Context, w io.Writer) error {
	return collabFragments.ExecuteTemplate(w, "card", cardView(c.t))
}

func (c cardBody) Render(_ context.Context, w io.Writer) error {
	return collabFragments.ExecuteTemplate(w, "card-body", cardView(c.t))
}

func cardView(t *collabTask) map[string]any {
	var assignable []*persona
	for _, p := range personas {
		if p.sees(t) {
			assignable = append(assignable, p)
		}
	}
	return map[string]any{
		"T": t, "ID": cardID(t.ID), "Columns": columns, "Titles": columnTitles, "Assignable": assignable,
	}
}

func (c *collab) renderColumnLocked(r *role, col string) string {
	var b strings.Builder
	for _, t := range c.sortedLocked() {
		if t.Column == col && r.sees(t) {
			b.WriteString(tavern.RenderComponent(card{t}))
		}
	}
	return b.String()
}

func (c *collab) sortedLocked() []*collabTask {
	out := make([]*collabTask, 0, len(c.tasks))
	for id := 1; id < c.nextID; id++ {
		if t, ok := c.tasks[id]; ok {
			out = append(out, t)
		}
	}
	return out
}

func (c *collab) renderActivityLocked(r *role) string {
	var entries []string
	for _, e := range c.activity {
		if text := e.textFor(r); text != "" {
			entries = append(entries, text)
		}
	}
	return execString("activity", entries)
}

func (c *collab) renderStatsLocked(r *role) string {
	counts := map[string]int{}
	for _, t := range c.tasks {
		if r.sees(t) {
			counts[t.Column]++
		}
	}
	return execString("stats", map[string]any{"Columns": columns, "Titles": columnTitles, "Counts": counts})
}

func renderNotice(n notice) string {
	return execString("notice", map[string]any{"ID": noticeID(n.ID), "N": n})
}

func execString(name string, data any) string {
	var b strings.Builder
	if err := collabFragments.ExecuteTemplate(&b, name, data); err != nil {
		return "<!-- render error -->"
	}
	return b.String()
}

var collabFragments = template.Must(template.New("").Parse(collabFragmentsHTML))
