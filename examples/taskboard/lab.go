package main

import (
	"errors"
	"fmt"
	"html/template"
	"strings"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// The interaction lab is a small task list shown in three regions, each on
// its own stream:
//
//   - votes: hot vote buttons sent as tavern-js commands on pointerdown,
//     protected by pause-on-pointerdown;
//   - editor: live title editing, protected by pause-on-pointerdown and
//     defer-on-focus;
//   - log: the server's record of processed commands, with click commands.
//
// Every stream is scoped to its connection, so the server can send one
// window a fresh snapshot after its protections release (resync). The
// snapshot travels on the same stream as live changes, so it can never be
// overtaken by an older change.
const (
	labRegionVotes  = "votes"
	labRegionEditor = "editor"
	labRegionLog    = "log"

	labLogLimit   = 10
	labBurstMax   = 20
	labBurstEvery = 150 * time.Millisecond
)

var labRegions = []string{labRegionVotes, labRegionEditor, labRegionLog}

func labTopic(region string) string { return "lab/" + region }

type labTask struct {
	ID       int
	Title    string
	TitleRev int // bumped by every title change; edits must start from the current one
	Votes    int
	Internal bool
}

// labConn is one open region stream.
type labConn struct {
	p      *persona
	region string
}

type lab struct {
	mu     sync.Mutex
	broker *tavern.SSEBroker
	nextID int
	tasks  map[int]*labTask
	log    []activityEntry     // newest first, bounded
	conns  map[string]*labConn // stream key → connection, while open
	burst  bool                // a simulated burst is running
	done   <-chan struct{}     // closed at shutdown
}

func newLab(b *tavern.SSEBroker, done <-chan struct{}) *lab {
	l := &lab{broker: b, nextID: 1, tasks: map[int]*labTask{}, conns: map[string]*labConn{}, done: done}
	for _, seed := range []struct {
		title    string
		internal bool
	}{
		{"Pick a release name", false},
		{"Draft the launch post", false},
		{"Decide the budget (leads only)", true},
	} {
		l.tasks[l.nextID] = &labTask{ID: l.nextID, Title: seed.title, TitleRev: 1, Internal: seed.internal}
		l.nextID++
	}
	return l
}

func labKey(conn, region string) string { return region + "/" + conn }

func (l *lab) visible(p *persona, id int) (*labTask, error) {
	t, ok := l.tasks[id]
	if !ok || !p.role.allows(t.Internal) {
		return nil, errNotFound
	}
	return t, nil
}

// task returns a copy of a task p may see.
func (l *lab) task(p *persona, id int) (*labTask, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, err := l.visible(p, id)
	if err != nil {
		return nil, err
	}
	cp := *t
	return &cp, nil
}

func (l *lab) sorted() []*labTask {
	out := make([]*labTask, 0, len(l.tasks))
	for id := 1; id < l.nextID; id++ {
		if t, ok := l.tasks[id]; ok {
			out = append(out, t)
		}
	}
	return out
}

// subscribe opens a region stream for conn. Like the collaboration board,
// the snapshot is taken and the stream registered under mu, so no change
// falls between them.
func (l *lab) subscribe(p *persona, region, conn string, status *tavern.SubscriptionStatus) (msgs <-chan string, unsubscribe func()) {
	key := labKey(conn, region)
	l.mu.Lock()
	defer l.mu.Unlock()
	ch, unsub := l.broker.SubscribeWith(labTopic(region),
		tavern.SubWithScope(key),
		tavern.SubWithSnapshot(func() string { return l.snapshotLocked(p, region) }),
		tavern.SubWithDisconnectOnLoss(),
		tavern.SubWithStatus(status),
	)
	if ch == nil {
		return nil, nil
	}
	c := &labConn{p: p, region: region}
	l.conns[key] = c
	return ch, func() {
		unsub()
		l.mu.Lock()
		if l.conns[key] == c {
			delete(l.conns, key)
		}
		l.mu.Unlock()
	}
}

// resync sends conn's region a fresh snapshot on its own stream. The
// stream must be p's.
func (l *lab) resync(p *persona, conn, region string) error {
	key := labKey(conn, region)
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.conns[key]
	if !ok || c.p != p {
		return errNotFound
	}
	l.broker.PublishTo(labTopic(region), key, l.snapshotLocked(c.p, region))
	return nil
}

// publishTaskLocked sends a task's change to every open task-region stream
// allowed to see it. Cards are rendered per viewer because their command
// URLs name the viewer.
func (l *lab) publishTaskLocked(t *labTask, frags func(region string, v labView) []tavern.Fragment) {
	for key, c := range l.conns {
		if c.region == labRegionLog || !c.p.role.allows(t.Internal) {
			continue
		}
		l.broker.PublishTo(labTopic(c.region), key, labUpdate(tavern.RenderFragments(frags(c.region, labView{T: t, P: c.p})...)))
	}
}

// labView is what a card template needs.
type labView struct {
	T *labTask
	P *persona
}

// recordTaskLocked logs an action on t, shown only to roles that may see t.
func (l *lab) recordTaskLocked(t *labTask, text string) {
	entry := activityEntry{Text: text}
	if !t.Internal {
		entry.PublicText = text
	}
	l.recordLocked(entry)
}

// recordLocked logs an entry and sends each log stream its role's view,
// skipping streams whose view did not change.
func (l *lab) recordLocked(entry activityEntry) {
	stamp := time.Now().Format("15:04:05") + "  "
	entry.Text = stamp + entry.Text
	if entry.PublicText != "" {
		entry.PublicText = stamp + entry.PublicText
	}
	l.log = append([]activityEntry{entry}, l.log...)
	if len(l.log) > labLogLimit {
		l.log = l.log[:labLogLimit]
	}
	for key, c := range l.conns {
		if c.region == labRegionLog && entry.textFor(c.p.role) != "" {
			l.broker.PublishTo(labTopic(labRegionLog), key, labUpdate(tavern.RenderFragments(tavern.Replace("lab-log-list", l.renderLogLocked(c.p.role)))))
		}
	}
}

func publicEntry(text string) activityEntry { return activityEntry{Text: text, PublicText: text} }

func (l *lab) vote(p *persona, id int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, err := l.visible(p, id)
	if err != nil {
		return err
	}
	t.Votes++
	l.publishCardLocked(t)
	l.recordTaskLocked(t, fmt.Sprintf("%s voted for %q (%d)", p.name, t.Title, t.Votes))
	return nil
}

var (
	errConflict = errors.New("conflict")
	// errStale reports an edit that started from an older title.
	errStale = fmt.Errorf("%w: the title changed since you started editing", errConflict)
)

// rename saves a title edited from revision rev. The comparison and the
// write happen under mu, so of two saves from the same revision only the
// first succeeds.
func (l *lab) rename(p *persona, id, rev int, title string) (*labTask, error) {
	title, err := validTitle(title)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t, err := l.visible(p, id)
	if err != nil {
		return nil, err
	}
	if t.TitleRev != rev {
		cp := *t
		return &cp, errStale
	}
	t.Title = title
	t.TitleRev++
	l.publishCardLocked(t)
	l.recordTaskLocked(t, fmt.Sprintf("%s renamed a task to %q", p.name, t.Title))
	cp := *t
	return &cp, nil
}

func (l *lab) add(p *persona, title string) error {
	title, err := validTitle(title)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t := &labTask{ID: l.nextID, Title: title, TitleRev: 1}
	l.nextID++
	l.tasks[t.ID] = t
	l.publishTaskLocked(t, func(region string, v labView) []tavern.Fragment {
		if region == labRegionVotes {
			return []tavern.Fragment{tavern.Append("lab-votes-list", renderLab("vote-card", v))}
		}
		return []tavern.Fragment{tavern.Append("lab-edit-list", renderLab("edit-card", v))}
	})
	l.recordTaskLocked(t, fmt.Sprintf("%s added %q", p.name, t.Title))
	return nil
}

func (l *lab) remove(p *persona, id int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, err := l.visible(p, id)
	if err != nil {
		return err
	}
	delete(l.tasks, id)
	l.publishTaskLocked(t, func(region string, _ labView) []tavern.Fragment {
		if region == labRegionVotes {
			return []tavern.Fragment{tavern.Delete(fmt.Sprintf("lab-vote-%d", t.ID))}
		}
		return []tavern.Fragment{tavern.Delete(fmt.Sprintf("lab-edit-%d", t.ID))}
	})
	l.recordTaskLocked(t, fmt.Sprintf("%s deleted %q", p.name, t.Title))
	return nil
}

func (l *lab) clearLog(p *persona) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.log = nil
	l.recordLocked(publicEntry(p.name + " cleared the log"))
}

// publishCardLocked replaces the task's card in both task regions.
func (l *lab) publishCardLocked(t *labTask) {
	l.publishTaskLocked(t, func(region string, v labView) []tavern.Fragment {
		if region == labRegionVotes {
			return []tavern.Fragment{tavern.Replace(fmt.Sprintf("lab-vote-%d", t.ID), renderLab("vote-body", v))}
		}
		return []tavern.Fragment{tavern.Replace(fmt.Sprintf("lab-edit-%d", t.ID), renderLab("edit-body", v))}
	})
}

// startBurst simulates someone else voting: count votes, one every
// labBurstEvery, spread over the public tasks. Only one burst runs at a
// time and it stops at shutdown.
func (l *lab) startBurst(by *persona, count int) error {
	if count < 1 || count > labBurstMax {
		return fmt.Errorf("%w: count must be from 1 to %d", errInvalid, labBurstMax)
	}
	l.mu.Lock()
	if l.burst {
		l.mu.Unlock()
		return fmt.Errorf("%w: a burst is already running", errConflict)
	}
	l.burst = true
	l.recordLocked(publicEntry(fmt.Sprintf("%s started %d simulated votes", by.name, count)))
	l.mu.Unlock()

	go func() {
		defer func() {
			l.mu.Lock()
			l.burst = false
			l.mu.Unlock()
		}()
		tick := time.NewTicker(labBurstEvery)
		defer tick.Stop()
		for i := 0; i < count; i++ {
			select {
			case <-tick.C:
			case <-l.done:
				return
			}
			l.mu.Lock()
			var public []*labTask
			for _, t := range l.sorted() {
				if !t.Internal {
					public = append(public, t)
				}
			}
			if len(public) > 0 {
				t := public[i%len(public)]
				t.Votes++
				l.publishCardLocked(t)
			}
			l.mu.Unlock()
		}
	}()
	return nil
}

// Rendering.

func labUpdate(html string) string { return tavern.NewSSEMessage("lab-update", html).String() }

func (l *lab) snapshotLocked(p *persona, region string) string {
	var b strings.Builder
	switch region {
	case labRegionLog:
		b.WriteString(tavern.RenderFragments(tavern.Replace("lab-log-list", l.renderLogLocked(p.role))))
	default:
		card, list := "vote-card", "lab-votes-list"
		if region == labRegionEditor {
			card, list = "edit-card", "lab-edit-list"
		}
		var cards strings.Builder
		for _, t := range l.sorted() {
			if p.role.allows(t.Internal) {
				cards.WriteString(renderLab(card, labView{T: t, P: p}))
			}
		}
		b.WriteString(tavern.RenderFragments(tavern.Replace(list, cards.String())))
	}
	return tavern.NewSSEMessage("lab-snapshot", b.String()).String()
}

// renderLogLocked renders the entries r may see.
func (l *lab) renderLogLocked(r *role) string {
	var entries []string
	for _, e := range l.log {
		if text := e.textFor(r); text != "" {
			entries = append(entries, text)
		}
	}
	return renderLab("log", entries)
}

func renderLab(name string, data any) string {
	var b strings.Builder
	if err := labFragments.ExecuteTemplate(&b, name, data); err != nil {
		return "<!-- render error -->"
	}
	return b.String()
}

var labFragments = template.Must(template.New("").Parse(labFragmentsHTML))
