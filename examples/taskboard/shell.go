package main

import (
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
	mu        sync.Mutex
	broker    *tavern.SSEBroker
	notices   []string // newest first, bounded
	noticeN   int
	board     []string // newest first, bounded
	boardN    int
	deploys   int
	alerts    int
	lifelines map[string]*persona // connection id → persona, while its lifeline is open
	feedN     int

	boardStreams sync.Map // connection id → *droppableStream
	holds        holds
	done         <-chan struct{}
}

// boardStreamOptions are validated once at startup; the snapshot is added
// per stream.
func boardStreamOptions(status *tavern.SubscriptionStatus, snapshot func() string) []tavern.SubscribeOption {
	return []tavern.SubscribeOption{
		tavern.SubWithSnapshot(snapshot),
		tavern.SubWithDisconnectOnLoss(),
		tavern.SubWithStatus(status),
	}
}

func newShell(b *tavern.SSEBroker, done <-chan struct{}) *shell {
	if err := tavern.ValidateSubscribeOptions(tavern.TargetSubscribeMultiWith,
		boardStreamOptions(tavern.NewSubscriptionStatus(), func() string { return "" })...); err != nil {
		panic(err)
	}
	s := &shell{broker: b, lifelines: map[string]*persona{}, done: done}
	s.holds.ttl, s.holds.max = holdTTL, holdMax
	b.DefineGroup(shellOpsGroup, []string{topicOpsDeploys, topicOpsAlerts})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publishNoticesLocked()
	s.publishOpsLocked()
	for _, t := range shellChannels {
		s.publishChannel(t, "Nothing yet")
	}
	s.publishChannel(topicLeadsChannel, "Nothing yet")
	return s
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
			s.mu.Unlock()
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
	s.publishChannel(topic, fmt.Sprintf("%s  %s posted", time.Now().Format("15:04:05"), p.name))
	return nil
}

// publishChannel keeps a channel's latest post with PublishWithReplay, so a
// lifeline that starts following it receives that post at once.
func (s *shell) publishChannel(topic, text string) {
	s.broker.PublishWithReplay(topic, oob(channelID(topic), "channel", text))
}

func channelID(topic string) string { return strings.ReplaceAll(topic, "/", "-") }

func (s *shell) notifyRole(p *persona, r *role) {
	s.broker.PublishTo(topicShellRole, r.name, oob("shell-role-notice", "text", fmt.Sprintf("%s  %s sent a note to %ss", time.Now().Format("15:04:05"), p.name, strings.ToLower(r.label))))
}

// Board view.

// openBoard subscribes the board view. The snapshot is taken and the
// subscription registered under mu, and the board changes under mu.
func (s *shell) openBoard(status *tavern.SubscriptionStatus) (msgs <-chan tavern.TopicMessage, unsubscribe func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broker.SubscribeMultiWith([]string{topicBoardTasks, topicBoardStats},
		boardStreamOptions(status, func() string { return s.boardSnapshotLocked() })...)
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

// boardSnapshotLocked is sent once per topic of the board's subscription;
// both copies replace the whole view, so the second changes nothing.
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
	s.broker.PublishWithReplay(topicOpsAlerts, oob("shell-ops-alerts", "text", fmt.Sprint(s.alerts, " alerts")))
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
