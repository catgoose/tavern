// Command oteldemo registers a broker's metrics with an OpenTelemetry meter
// provider, collects them with an in-process manual reader and prints the
// Tavern instruments, then unregisters and shuts the provider down. It
// needs no collector or network.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/catgoose/tavern"
	"github.com/catgoose/tavern/tavernotel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func topic(set attribute.Set) string {
	if v, ok := set.Value("topic"); ok {
		return "{topic=" + v.AsString() + "}"
	}
	return ""
}

// collect returns one line per data point of the tavern instruments.
func collect(ctx context.Context, r *sdkmetric.ManualReader) ([]string, error) {
	var rm metricdata.ResourceMetrics
	if err := r.Collect(ctx, &rm); err != nil {
		return nil, err
	}
	var lines []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if !strings.HasPrefix(m.Name, "tavern.") {
				continue
			}
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, p := range d.DataPoints {
					lines = append(lines, fmt.Sprintf("%s%s %d", m.Name, topic(p.Attributes), p.Value))
				}
			case metricdata.Gauge[int64]:
				for _, p := range d.DataPoints {
					lines = append(lines, fmt.Sprintf("%s%s %d", m.Name, topic(p.Attributes), p.Value))
				}
			case metricdata.Gauge[float64]:
				for _, p := range d.DataPoints {
					lines = append(lines, fmt.Sprintf("%s%s %g", m.Name, topic(p.Attributes), p.Value))
				}
			}
		}
	}
	sort.Strings(lines)
	return lines, nil
}

func run(w io.Writer) error {
	ctx := context.Background()
	b := tavern.NewSSEBroker(tavern.WithMetrics())
	defer b.Close()
	orders, u1 := b.Subscribe("orders")
	chat, u2 := b.Subscribe("chat")
	for i := range 3 {
		b.Publish("orders", fmt.Sprintf("order %d", i))
		<-orders
	}
	b.Publish("chat", "hi")
	<-chat

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	// One topic gets its own attribute; the rest are summed under "other".
	unregister, err := tavernotel.Register(b, mp, tavernotel.WithMeterName("oteldemo"), tavernotel.WithMaxTopicCardinality(1))
	if err != nil {
		return err
	}
	lines, err := collect(ctx, reader)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "collected after 3 orders and 1 chat (%d data points):\n", len(lines))
	for _, l := range lines {
		fmt.Fprintln(w, "  "+l)
	}
	unregister()
	u1()
	u2()
	lines, err = collect(ctx, reader)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "collected after unregister: %d data points\n", len(lines))
	if err := mp.Shutdown(ctx); err != nil {
		return err
	}
	fmt.Fprintln(w, "meter provider shut down")
	return nil
}
