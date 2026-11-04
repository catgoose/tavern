package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/catgoose/tavern"
)

// The app shell keeps one lifeline stream per window and swaps views in and
// out below it, each with its own stream:
//
//   - lifeline: SubscribeMultiWithMeta on the global notices topic, plus
//     channels the server adds and removes with AddTopic/RemoveTopic. A
//     role-scoped subscription with the same subscriber ID carries role
//     notices and lets AddTopicForScope reach every lifeline of a role.
//   - board view: SubscribeMultiWith over two topics, with a snapshot and
//     loss-aware termination.
//   - feeds view: SubscribeGlobWith over shell/feeds/**.
//   - ops view: a static DefineGroup served by GroupHandler.
const (
	topicShellNotices = "shell/notices"
	topicShellRole    = "shell/role"
	topicBoardTasks   = "shell/board/tasks"
	topicBoardStats   = "shell/board/stats"
	shellFeedPattern  = "shell/feeds/**"
	shellOpsGroup     = "shell-ops"
	topicOpsDeploys   = "shell/ops/deploys"
	topicOpsAlerts    = "shell/ops/alerts"

	shellNoticeLimit = 6
	shellBoardLimit  = 8
	shellFeedLimit   = 8
)

// shellChannels are the topics a window may follow on its lifeline. Only
// these names are accepted; the leads channel is added by role.
var shellChannels = map[string]string{
	"releases":  "shell/channel/releases",
	"incidents": "shell/channel/incidents",
}

const topicLeadsChannel = "shell/channel/leads"

// shellFeedTopics are the topics the feeds demo can publish to: two match
// the view's glob, one does not.
var shellFeedTopics = map[string]string{
	"web":   "shell/feeds/releases/web",
	"api":   "shell/feeds/releases/api",
	"noise": "shell/other/noise",
}

var shellViews = []string{"board", "feeds", "ops"}

type shell struct {
	mu      sync.Mutex
	broker  *tavern.SSEBroker
	notices []string // newest first, bounded
	noticeN int
	board   []string // newest first, bounded
	boardN  int
	deploys int
	alerts  int
	// alertsKnown is false after "forget the alerts count": the ops view
	// then has no alerts value until the next alert.
	alertsKnown bool
	lifelines   map[string]*persona // connection id → persona, while its lifeline is open
	channels    map[string]string   // channel topic → latest post
	roleNotes   map[string]string   // role name → latest note
	feedN       int

	boardStreams sync.Map // connection id → *droppableStream
	pausers      sync.Map // connection id → *pauser owned by its open lifeline, shared with its ops view
	holds        holds
	repair       feedRepair
	done         <-chan struct{}
}

// boardStreamOptions are the board view's subscription options, validated
// once at startup. Its snapshot is not one of them: see openBoard.
func boardStreamOptions(status *tavern.SubscriptionStatus) []tavern.SubscribeOption {
	return []tavern.SubscribeOption{
		tavern.SubWithDisconnectOnLoss(),
		tavern.SubWithStatus(status),
	}
}

func newShell(b *tavern.SSEBroker, done <-chan struct{}) *shell {
	if err := tavern.ValidateSubscribeOptions(tavern.TargetSubscribeMultiWith,
		boardStreamOptions(tavern.NewSubscriptionStatus())...); err != nil {
		panic(err)
	}
	s := &shell{
		broker: b, lifelines: map[string]*persona{}, channels: map[string]string{},
		roleNotes: map[string]string{}, alertsKnown: true, done: done,
	}
	s.holds.ttl, s.holds.max = holdTTL, holdMax
	s.repair.init(shellRepairs, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.republishLocked()
	})
	b.DefineGroup(shellOpsGroup, []string{topicOpsDeploys, topicOpsAlerts})
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range shellChannels {
		s.channels[t] = "Nothing yet"
	}
	s.channels[topicLeadsChannel] = "Nothing yet"
	s.republishLocked()
	return s
}

// shellRepairs reports the topics whose dropped messages are repaired: the
// lifeline's and the ops view's, which are whole representations. Board
// streams are loss-aware instead, and feed items are live only.
func shellRepairs(topic string) bool {
	return topic == topicShellNotices || topic == topicShellRole ||
		strings.HasPrefix(topic, "shell/channel/") || strings.HasPrefix(topic, "shell/ops/")
}

// republishLocked publishes the current notices, channel posts, role notes
// and ops counts.
func (s *shell) republishLocked() {
	s.publishNoticesLocked()
	for topic, text := range s.channels {
		s.broker.PublishWithReplay(topic, oob(channelID(topic), "channel", text))
	}
	for name, note := range s.roleNotes {
		s.broker.PublishTo(topicShellRole, name, oob("shell-role-notice", "text", note))
	}
	s.publishOpsLocked()
}

// Lifeline.

// openLifeline subscribes a window's lifeline. The connection id must be new:
// it is the subscriber ID that AddTopic targets.
func (s *shell) openLifeline(p *persona, conn string) (msgs <-chan tavern.TopicMessage, unsubscribe func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if conn == "" || s.lifelines[conn] != nil {
		return nil, nil, fmt.Errorf("%w: that window already has a lifeline", errConflict)
	}
	meta := tavern.SubscribeMeta{ID: conn, Meta: map[string]string{"persona": p.id}}
	multi, unsubMulti := s.broker.SubscribeMultiWithMeta(meta, topicShellNotices)
	role, unsubRole := s.broker.SubscribeWith(topicShellRole, tavern.SubWithScope(p.role.name), tavern.SubWithMeta(meta))
	if role == nil {
		unsubMulti()
		return nil, nil, errAdmission
	}
	s.lifelines[conn] = p
	gate := &pauser{resume: make(chan struct{})}
	s.pausers.Store(conn, gate)

	// Merge both into one stream. It ends when either source closes (the
	// broker closed or dropped it) or when the handler unsubscribes.
	out := make(chan tavern.TopicMessage, 16)
	stop := make(chan struct{})
	var stopOnce sync.Once
	halt := func() { stopOnce.Do(func() { close(stop) }) }
	var wg sync.WaitGroup
	forward := func(src func() (tavern.TopicMessage, bool)) {
		defer wg.Done()
		defer halt()
		for {
			m, ok := src()
			if !ok {
				return
			}
			select {
			case out <- m:
			case <-stop:
				return
			}
		}
	}
	wg.Add(2)
	go forward(func() (tavern.TopicMessage, bool) {
		select {
		case m, ok := <-multi:
			return m, ok
		case <-stop:
			return tavern.TopicMessage{}, false
		}
	})
	go forward(func() (tavern.TopicMessage, bool) {
		select {
		case m, ok := <-role:
			return tavern.TopicMessage{Topic: topicShellRole, Data: m}, ok
		case <-stop:
			return tavern.TopicMessage{}, false
		}
	})
	closed := make(chan struct{})
	go func() {
		wg.Wait()
		close(out)
		close(closed)
	}()
	var once sync.Once
	return out, func() {
		once.Do(func() {
			halt()
			unsubMulti()
			unsubRole()
			<-closed
			s.mu.Lock()
			if s.lifelines[conn] == p {
				delete(s.lifelines, conn)
			}
			s.pausers.CompareAndDelete(conn, gate)
			s.mu.Unlock()
			// A write still waiting on this lifeline's pause would wait
			// forever: nothing can reach the gate any more.
			gate.set(false)
		})
	}, nil
}

// owner checks that conn's lifeline is open and belongs to p.
func (s *shell) owner(p *persona, conn string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lifelines[conn] != p {
		return errNotFound
	}
	return nil
}

// follow adds or removes an allowlisted channel on a window's lifeline.
func (s *shell) follow(p *persona, conn, channel string, on bool) (bool, error) {
	topic, ok := shellChannels[channel]
	if !ok {
		return false, fmt.Errorf("%w: unknown channel", errInvalid)
	}
	if err := s.owner(p, conn); err != nil {
		return false, err
	}
	if on {
		return s.broker.AddTopic(conn, topic, true), nil
	}
	return s.broker.RemoveTopic(conn, topic, true), nil
}

// leadsChannel adds or removes the leads channel on every lead's lifeline.
func (s *shell) leadsChannel(p *persona, on bool) (int, error) {
	if !p.role.seesInternal {
		return 0, fmt.Errorf("%w: only leads can change the leads channel", errForbidden)
	}
	if on {
		return s.broker.AddTopicForScope(roleLead.name, topicLeadsChannel, true), nil
	}
	return s.broker.RemoveTopicForScope(roleLead.name, topicLeadsChannel, true), nil
}

func (s *shell) postNotice(p *persona) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noticeN++
	s.notices = append([]string{fmt.Sprintf("%s  %s posted notice %d", time.Now().Format("15:04:05"), p.name, s.noticeN)}, s.notices...)
	if len(s.notices) > shellNoticeLimit {
		s.notices = s.notices[:shellNoticeLimit]
	}
	s.publishNoticesLocked()
}

func (s *shell) publishNoticesLocked() {
	s.broker.PublishWithReplay(topicShellNotices, oob("shell-notices", "notices", s.notices))
}

func (s *shell) postChannel(p *persona, channel string) error {
	topic, ok := shellChannels[channel]
	if channel == "leads" {
		if !p.role.seesInternal {
			return fmt.Errorf("%w: only leads can post to the leads channel", errForbidden)
		}
		topic, ok = topicLeadsChannel, true
	}
	if !ok {
		return fmt.Errorf("%w: unknown channel", errInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	text := fmt.Sprintf("%s  %s posted", time.Now().Format("15:04:05"), p.name)
	s.channels[topic] = text
	// PublishWithReplay keeps the latest post, so a lifeline that starts
	// following the channel receives it at once.
	s.broker.PublishWithReplay(topic, oob(channelID(topic), "channel", text))
	return nil
}

func channelID(topic string) string { return strings.ReplaceAll(topic, "/", "-") }

func (s *shell) notifyRole(p *persona, r *role) {
	s.mu.Lock()
	defer s.mu.Unlock()
	note := fmt.Sprintf("%s  %s sent a note to %ss", time.Now().Format("15:04:05"), p.name, strings.ToLower(r.label))
	s.roleNotes[r.name] = note
	s.broker.PublishTo(topicShellRole, r.name, oob("shell-role-notice", "text", note))
}

// Board view.

// openBoard subscribes the board view and returns its snapshot, to be
// written once before any message. Both are taken under mu, and the board
// changes under mu, so every change is either in the snapshot or delivered
// after it. A per-topic SubWithSnapshot would queue one copy per topic, and
// the merged stream could deliver the second copy after a newer change.
func (s *shell) openBoard(status *tavern.SubscriptionStatus) (snapshot string, msgs <-chan tavern.TopicMessage, unsubscribe func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	msgs, unsubscribe = s.broker.SubscribeMultiWith([]string{topicBoardTasks, topicBoardStats}, boardStreamOptions(status)...)
	return s.boardSnapshotLocked(), msgs, unsubscribe
}

func (s *shell) addBoardTask(p *persona) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.boardN++
	s.board = append([]string{fmt.Sprintf("%s's task %d", p.name, s.boardN)}, s.board...)
	if len(s.board) > shellBoardLimit {
		s.board = s.board[:shellBoardLimit]
	}
	s.broker.Publish(topicBoardTasks, oob("shell-board-tasks", "board-tasks", s.board))
	s.broker.Publish(topicBoardStats, oob("shell-board-stats", "text", fmt.Sprint(s.boardN, " tasks added")))
}

func (s *shell) boardSnapshotLocked() string {
	html := oob("shell-board-tasks", "board-tasks", s.board) + oob("shell-board-stats", "text", fmt.Sprint(s.boardN, " tasks added"))
	return tavern.NewSSEMessage("shell-board-snapshot", html).String()
}

// Feeds view.

func (s *shell) publishFeed(p *persona, key string) error {
	topic, ok := shellFeedTopics[key]
	if !ok {
		return fmt.Errorf("%w: unknown feed", errInvalid)
	}
	s.mu.Lock()
	s.feedN++
	n := s.feedN
	s.mu.Unlock()
	s.broker.Publish(topic, fmt.Sprintf("%s posted item %d", p.name, n))
	return nil
}

// Ops view.

func (s *shell) bumpOps(kind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case "deploy":
		s.deploys++
	case "alert":
		s.alerts++
		s.alertsKnown = true
	default:
		return fmt.Errorf("%w: unknown ops event", errInvalid)
	}
	s.publishOpsLocked()
	return nil
}

// publishOpsLocked publishes the ops counts whole, with PublishWithReplay, so
// a new group stream starts from the latest of each topic.
func (s *shell) publishOpsLocked() {
	s.broker.PublishWithReplay(topicOpsDeploys, oob("shell-ops-deploys", "text", fmt.Sprint(s.deploys, " deploys")))
	if s.alertsKnown {
		s.broker.PublishWithReplay(topicOpsAlerts, oob("shell-ops-alerts", "text", fmt.Sprint(s.alerts, " alerts")))
	}
}

// forgetAlerts drops the alerts count until the next alert, so an ops view
// opened meanwhile has only one of its two values.
func (s *shell) forgetAlerts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alertsKnown = false
	s.broker.ClearReplay(topicOpsAlerts)
}

// waitPaused waits while conn's current lifeline has paused its streams.
// Each write looks the gate up again, so a stream that outlives one
// lifeline follows the next one's control.
func (s *shell) waitPaused(ctx context.Context, conn string, done <-chan struct{}) {
	if v, ok := s.pausers.Load(conn); ok {
		v.(*pauser).wait(ctx, done)
	}
}

// Diagnostics: what the server holds for a window, read from the broker.

type shellDiagnostics struct {
	LifelineTopics []string       `json:"lifelineTopics"`
	BoardStreams   int            `json:"boardStreams"`
	TopicCounts    map[string]int `json:"topicCounts"`
}

func (s *shell) diagnostics(conn string) shellDiagnostics {
	d := shellDiagnostics{LifelineTopics: s.broker.SubscriberTopics(conn), TopicCounts: map[string]int{}}
	slices.Sort(d.LifelineTopics)
	if _, ok := s.boardStreams.Load(conn); ok {
		d.BoardStreams = 1
	}
	for topic, n := range s.broker.TopicCounts() {
		if strings.HasPrefix(topic, "shell/") && n > 0 {
			d.TopicCounts[topic] = n
		}
	}
	return d
}

// Rendering.

// oob renders a template as the content of an OOB Replace of id.
func oob(id, name string, data any) string {
	return tavern.RenderFragments(tavern.Replace(id, renderShell(name, data)))
}

// shellFrame names an SSE event after its topic, as group handlers do.
func shellFrame(m tavern.TopicMessage) string {
	if strings.HasPrefix(m.Data, "event: ") {
		return m.Data // control events and preformatted frames
	}
	return tavern.NewSSEMessage(m.Topic, m.Data).String()
}

func renderShell(name string, data any) string {
	var b strings.Builder
	if err := shellFragments.ExecuteTemplate(&b, name, data); err != nil {
		return "<!-- render error -->"
	}
	return b.String()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

var errAdmission = errors.New("too many connections")

var shellFragments = template.Must(template.New("").Parse(shellFragmentsHTML))
