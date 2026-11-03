// Command capacity runs bounded, repeatable load scenarios against Tavern
// and prints what it measured. See README.md in this directory.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// presets are the scenarios used for the published report.
var presets = map[string]Scenario{
	"fanout": {Name: "fanout", Transport: "broker", Subscribers: 2000, Topics: 1, Messages: 200,
		PayloadBytes: 512, Buffer: 256, Timeout: 30 * time.Second},
	"slow-reader": {Name: "slow-reader", Transport: "broker", Subscribers: 500, Topics: 1, Messages: 200,
		PayloadBytes: 512, Buffer: 64, Slow: 50, LossAware: true, Timeout: 30 * time.Second},
	"multi-key-burst": {Name: "multi-key-burst", Transport: "broker", Subscribers: 1000, Topics: 50, Messages: 100,
		PayloadBytes: 256, Buffer: 32, Timeout: 30 * time.Second},
	"replay-reconnect": {Name: "replay-reconnect", Transport: "broker", Subscribers: 1000, Topics: 10, Messages: 100,
		PayloadBytes: 1024, Buffer: 128, Replay: 100, ReconnectBurst: 1000, Timeout: 30 * time.Second},
	"replay-gap": {Name: "replay-gap", Transport: "broker", Subscribers: 200, Topics: 4, Messages: 200,
		PayloadBytes: 256, Buffer: 256, Replay: 50, ReconnectBurst: 200, Timeout: 30 * time.Second},
	"http-fanout": {Name: "http-fanout", Transport: "http", Subscribers: 200, Topics: 1, Messages: 200,
		PayloadBytes: 512, Buffer: 256, Timeout: 30 * time.Second},
	"http-slow-reconnect": {Name: "http-slow-reconnect", Transport: "http", Subscribers: 200, Topics: 4, Messages: 100,
		PayloadBytes: 1024, Buffer: 64, Replay: 100, Slow: 20, LossAware: true, ReconnectBurst: 200, Timeout: 30 * time.Second},
}

func main() {
	var s Scenario
	preset := flag.String("preset", "", "run named presets (comma separated, or \"all\") instead of the flags below")
	jsonOut := flag.Bool("json", false, "print one JSON report per line")
	flag.StringVar(&s.Name, "name", "custom", "scenario name")
	flag.StringVar(&s.Transport, "transport", "broker", "broker or http")
	flag.IntVar(&s.Subscribers, "subscribers", 100, "reading subscribers")
	flag.IntVar(&s.Topics, "topics", 1, "distinct topics (keys)")
	flag.IntVar(&s.Messages, "messages", 100, "messages per topic")
	flag.IntVar(&s.PayloadBytes, "payload", 256, "message size in bytes")
	flag.IntVar(&s.Buffer, "buffer", 64, "queue length per subscriber")
	flag.IntVar(&s.Replay, "replay", 0, "replay entries kept per topic")
	flag.IntVar(&s.Slow, "slow", 0, "subscribers that never read")
	flag.BoolVar(&s.LossAware, "loss-aware", false, "end subscribers on queue loss")
	flag.IntVar(&s.ReconnectBurst, "reconnect-burst", 0, "readers that reconnect at once after publishing (needs -replay)")
	flag.DurationVar(&s.Timeout, "timeout", 30*time.Second, "bound on waiting for delivery")
	flag.Parse()

	scenarios := []Scenario{s}
	if *preset != "" {
		scenarios = nil
		names := strings.Split(*preset, ",")
		if *preset == "all" {
			names = []string{"fanout", "slow-reader", "multi-key-burst", "replay-reconnect", "replay-gap", "http-fanout", "http-slow-reconnect"}
		}
		for _, n := range names {
			p, ok := presets[n]
			if !ok {
				fmt.Fprintf(os.Stderr, "unknown preset %q\n", n)
				os.Exit(2)
			}
			scenarios = append(scenarios, p)
		}
	}

	var reports []Report
	for _, sc := range scenarios {
		r, err := Run(sc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", sc.Name, err)
			os.Exit(1)
		}
		reports = append(reports, r)
		if *jsonOut {
			_ = json.NewEncoder(os.Stdout).Encode(r)
		}
	}
	if !*jsonOut {
		printTable(reports)
	}
}

func printTable(reports []Report) {
	if len(reports) == 0 {
		return
	}
	env := reports[0].Env
	fmt.Printf("%s %s/%s, %d CPUs, %s\n\n", env.Go, env.OS, env.Arch, env.CPUs, env.CPU)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "scenario\tpublish s\tfan-out\tdrops\treceived\tp50 ms\tp99 ms\treplayed\tgaps\theap MB\tworst case MB\tgoroutines\tterminations\t")
	for _, r := range reports {
		fmt.Fprintf(w, "%s\t%.3f\t%d\t%d\t%d\t%.2f\t%.2f\t%d\t%d\t%.1f\t%.1f\t%d→%d\t%v\t\n",
			r.Scenario.Name, r.PublishSeconds, r.FanoutAttempts, r.QueueDrops, r.Received,
			r.LatencyP50Ms, r.LatencyP99Ms, r.ReconnectReplayed, r.ReconnectGaps,
			r.HeapPublishedMB-r.HeapBaselineMB, r.WorstCaseQueueMB, r.GoroutinesStart, r.GoroutinesEnd, r.Terminations)
	}
	_ = w.Flush()
}
