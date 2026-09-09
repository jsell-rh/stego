The generated sweep controller moves page scheduling, cursors, worker bounds,
pass deadlines, and group rotation into STEGO. Hypershell supplies service-account
state groups, storage filters, and recovery actions. It retains the rules for
expiry, creator access, role limits, and provider identity.

The application regression test uses 17 deleted records. The first eight provider
calls remain slow until their contexts end. The former implementation reset a
short page even when it had not dispatched the remaining records. The test failed
after 20.41 seconds because later records never received a turn. With the generated
sweep, the same test passed in 13.32 seconds. The final stream-rotation checks also passed through the application and real
identity provider.

The runtime validates a complete page before actions begin. It uses a fixed
worker pool, records which actions started, joins all workers, and advances only
the contiguous started prefix. A short page keeps its cursor when the pass is
partial. Failure does not remove retained work. A later cycle tries it again.

A second generated test showed that a slow first stream could consume every
pass and prevent history from running. The next pass now begins with the next
stream when the budget ends. This retains the full operation budget and gives
both streams a turn. The application initially places current work before
history. Groups keep their declared rotation order.

Cursor progress is not a durable acknowledgment or proof of an external effect.
A failed callback can follow a completed external write. Recovery actions must
therefore tolerate repeats and enforce current domain rules. The source owns
cursor order, including database collation. STEGO checks page shape, bounded
cursor length, duplicates, immediate cursor repeats, and the configured cycle
page limit. It does not claim a consistent snapshot of mutable storage.

The initial application uses nine groups, twelve streams, eight workers, pages
of 100 records, a four-second group budget, and a one-second interval after a
pass. Each stream permits at most 10,000 data-page advances per cycle. This is a
bound, not a production capacity claim. Restart resets in-memory cursors and
recovers from retained database records.

A provider or storage outage remains retryable. An invalid recovery contract
stops the worker. Notices report group, stream, and attempt counts without logging
provider error messages or credentials. The scheduler does not create accounts,
raise roles, rebuild credentials, or infer deletion from a missing live read.

A local dispatch benchmark used Go 1.26.8 on Linux amd64 with an Intel Core Ultra
9 185H. Each sample ran for 200 ms without the race detector. Each operation
created a cancelable context and dispatched 100 empty actions through a new fixed
worker pool. Three samples produced these ranges:

| Workers | Time per page | Allocated bytes per page | Allocations per page |
| --- | --- | --- | --- |
| 1 | 29.2–31.3 microseconds | 2,456–2,457 | 9 |
| 8 | 40.2–47.8 microseconds | 3,973–4,005 | 23 |
| 32 | 53.2–56.5 microseconds | 9,194–9,222 | 71 |

This benchmark measures dispatch overhead. It excludes page validation, database
queries, provider calls, idle intervals, and group deadlines. More workers add
overhead for empty actions; these results do not select a production worker
count or establish application throughput. Hypershell retains its prior limit of
eight concurrent recovery actions.
