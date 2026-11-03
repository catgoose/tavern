# Capacity exercise

`capacity` runs bounded, repeatable load scenarios against Tavern and
reports what it measured. It is a way to size *your* workload, not a
benchmark of Tavern in general.

```sh
go run ./examples/capacity -preset all           # every preset, as a table
go run ./examples/capacity -preset fanout -json  # one JSON report per line
go run ./examples/capacity -h                    # define a custom scenario
```

Each scenario sets subscribers (spread over `-topics` keys), messages per
topic, payload size, queue length (`-buffer`), replay entries per topic,
subscribers that never read (`-slow`), loss-aware termination, and a
reconnect burst that resumes from the first message with `Last-Event-ID`.

- **`broker`** transport: in-process subscribers reading their channels.
- **`http`** transport: real SSE clients over loopback HTTP/1.1, through
  `StreamSSE` with a 2s write timeout; slow clients are TCP connections that
  never read.

## What it reports

| Field | Meaning |
|---|---|
| fan-out | messages × subscribers on the topic when published: queue attempts |
| drops | messages not queued because a queue was full (`PublishDrops`) |
| received | messages read by subscribers (broker) or SSE frames parsed by clients (http) |
| p50 / p99 ms | publish-to-read time for received messages |
| replayed / gaps | reconnect burst: replayed entries, and resumes whose ID had left the log |
| heap MB | live heap right after publishing, minus the baseline, after a GC |
| worst case MB | (subscribers + slow) × buffer × payload, if every queued message were a separate copy |
| goroutines | count before → after teardown |
| terminations | how every subscription ended (`SubWithStatus` reason) |

Counts describe Tavern's queues and, for http, frames the test clients
parsed. They are not browser delivery or rendering.

## Results

Three runs of `go run ./examples/capacity -preset all -json` on
go1.27.3 linux/amd64, Intel Core i9-10900X (10 cores, 20 threads), with
nothing else under load. Ranges cover the three runs.

| Scenario | Fan-out | Drops | Received | p50 ms | p99 ms | Heap MB | Worst case MB |
|---|---|---|---|---|---|---|---|
| fanout: 2000 subs, 200 × 512 B, buffer 256 | 400,000 | 0 | 400,000 | 0.58–0.64 | 2.1–2.8 | 15.2 | 250 |
| slow-reader: 500 + 50 non-reading, loss-aware, buffer 64 | 103,250 | 50 | 100,000 | 0.16–0.21 | 0.8–1.0 | 1.9 | 17 |
| multi-key-burst: 1000 subs over 50 keys, 100 × 256 B, buffer 32 | 100,000 | 0 | 100,000 | 0.03 | 0.2–0.5 | 2.1 | 7.8 |
| replay-reconnect: 1000 subs, replay 100, burst of 1000 | 100,000 | 0 | 100,000 | 0.06 | 0.8–1.5 | 5.0 | 125 |
| replay-gap: 200 subs, 200 messages, replay 50, burst of 200 | 40,000 | 0 | 40,000 | 0.04–0.05 | 0.3–0.5 | 1.5 | 12.5 |
| http-fanout: 200 clients, 200 × 512 B | 40,000 | 0 | 40,000 | 1.9–3.4 | 16.6–19.3 | 23.6–27.1 | 25 |
| http-slow-reconnect: 200 clients + 20 non-reading, loss-aware, buffer 64, burst of 200 | 21,475–21,970 | 1–24 | 19,562–19,969 | 0.8–2.6 | 13.7–16.8 | 19.7–22.2 | 13.8 |

Every run ended with every subscription accounted for and goroutines back
to the starting count. Reconnect results: replay-reconnect replayed all
99,000 entries after ID 1 with no gaps; replay-gap reported 200 gaps and
replayed nothing, because ID 1 had left a 50-entry log; http-slow-reconnect
replayed exactly 12,800 entries, 64 per resume, because replay is queued
into the 64-entry buffer.

What the runs show:

- **Message count is the bound, not bytes.** Queues are limited by length.
  A published string is shared by every queue that holds it, so 2000
  subscribers × 256 queued 512-byte messages used about 15 MB of heap, not
  the 250 MB a per-subscriber copy would need. Budget bytes as
  *distinct messages in flight × size*. Use the worst-case column if your
  messages are rendered per subscriber (filters that transform, scoped
  renders), because those copies are not shared.
- **Loss-aware subscriptions end at their first drop.** In slow-reader, each
  of the 50 non-reading subscribers ended with `queue-loss` after exactly one
  drop, while readers lost nothing.
- **Kernel buffers hide slow HTTP clients.** In http-slow-reconnect each
  non-reading TCP client was sent about 100 KB, which mostly fit in socket
  buffers, so between 1 and 24 subscriptions ended with `queue-loss` per run
  instead of all 20 slow ones, and some of those were reading clients that
  fell behind the burst. Over a network, slow clients show up later and less
  predictably than in-process.
- **Replay delivered on a resume is capped by the queue.** Size `-buffer`
  for the replay you expect, or expect `tavern-replay-truncated`.

## Limits

One machine, loopback networking, a publisher in a tight loop, and test
clients that parse frames immediately. No proxy, TLS, browser or
rendering. Heap figures are point-in-time `HeapAlloc` after a GC, not RSS.
Run the presets, then a scenario shaped like your traffic, on hardware like
your deployment before drawing conclusions.

`capacity_test.go` runs small versions in CI to keep the accounting and
teardown honest; it makes no performance assertions.
