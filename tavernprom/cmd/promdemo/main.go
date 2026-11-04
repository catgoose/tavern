// Command promdemo registers a broker's metrics with Prometheus, scrapes
// them in-process and prints the Tavern series, then unregisters and shows
// the series are gone. It needs no Prometheus server or network.
package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/tavernprom"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// scrape fetches the registry's exposition and returns the demo_ series.
func scrape(reg *prometheus.Registry) ([]string, error) {
	return scrapeHandler(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), "demo_")
}

// scrapeHandler fetches h's exposition and returns the series with prefix.
func scrapeHandler(h http.Handler, prefix string) ([]string, error) {
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), prefix) {
			lines = append(lines, sc.Text())
		}
	}
	return lines, sc.Err()
}

func run(w io.Writer) error {
	b := tavern.NewSSEBroker(tavern.WithMetrics(), tavern.WithObservability(tavern.ObservabilityConfig{PublishLatency: true, TopicThroughput: true}))
	defer b.Close()
	orders1, u1 := b.Subscribe("orders")
	orders2, u2 := b.Subscribe("orders")
	chat, u3 := b.Subscribe("chat")
	for i := range 5 {
		b.Publish("orders", fmt.Sprintf("order %d", i))
		<-orders1
		<-orders2
	}
	for i := range 2 {
		b.Publish("chat", fmt.Sprintf("hi %d", i))
		<-chat
	}
	time.Sleep(10 * time.Millisecond)

	reg := prometheus.NewRegistry()
	// One topic gets its own label; the rest are summed under "other".
	unregister, err := tavernprom.Register(b, reg, tavernprom.WithNamespace("demo"), tavernprom.WithMaxTopicCardinality(1))
	if err != nil {
		return err
	}
	lines, err := scrape(reg)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "scrape after 5 orders to 2 subscribers and 2 chats to 1 (%d demo_ series):\n", len(lines))
	for _, l := range lines {
		if !strings.Contains(l, "quantile") && !strings.Contains(l, "_bucket") {
			fmt.Fprintln(w, "  "+l)
		}
	}
	unregister()
	// Handler serves the broker's metrics from a registry of its own.
	own, err := scrapeHandler(tavernprom.Handler(b, tavernprom.WithNamespace("own")), "own_messages_published_total")
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "tavernprom.Handler, with its own registry:\n")
	for _, l := range own {
		fmt.Fprintln(w, "  "+l)
	}
	u1()
	u2()
	u3()
	lines, err = scrape(reg)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "scrape after unregister: %d demo_ series\n", len(lines))
	return nil
}
