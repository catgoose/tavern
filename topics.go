package tavern

// Topic name constants provided as conventions. These are common topic names
// used in dashboard and real-time UI applications. Your application may use any
// string as a topic name; these constants are provided for convenience and to
// encourage consistent naming across projects.
//
// See https://github.com/catgoose/tavern/issues/6 for discussion about moving
// app-specific constants to examples.
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
