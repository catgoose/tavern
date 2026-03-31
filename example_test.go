package tavern_test

import (
	"fmt"

	"github.com/catgoose/tavern"
)

// Topic name conventions for dashboard and real-time UI applications. Your
// application may use any string as a topic name; these are provided as
// examples of consistent naming patterns.
const (
	TopicSystemStats  = "system-stats"
	TopicDashMetrics  = "dashboard-metrics"
	TopicDashServices = "dashboard-services"
	TopicDashEvents   = "dashboard-events"
	TopicPeopleUpdate = "people-update"
	TopicActivityFeed = "activity-feed"
	TopicErrorTraces  = "error-traces"
	TopicThemeChange  = "theme-change"
	TopicCanvasUpdate = "canvas-update"
)

func ExampleSSEBroker_pubsub() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	ch, unsub := broker.Subscribe(TopicSystemStats)
	defer unsub()

	broker.Publish(TopicSystemStats, `{"cpu": 42}`)

	msg := <-ch
	fmt.Println(msg)
	// Output: {"cpu": 42}
}

func ExampleSSEBroker_Stats() {
	broker := tavern.NewSSEBroker()
	defer broker.Close()

	_, unsub1 := broker.Subscribe(TopicSystemStats)
	defer unsub1()
	_, unsub2 := broker.Subscribe(TopicActivityFeed)
	defer unsub2()

	stats := broker.Stats()
	fmt.Printf("topics=%d subscribers=%d drops=%d\n", stats.Topics, stats.Subscribers, stats.PublishDrops)
	// Output: topics=2 subscribers=2 drops=0
}
