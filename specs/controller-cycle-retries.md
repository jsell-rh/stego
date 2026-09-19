# Bounded action retries

The account cleanup test exposed a delay after early action failures. The
common scan retained each failure and continued to the end of the source.
Only the next full scan could retry those actions. A later scan remains the
recovery path when immediate retries cannot resolve an error.

`CycleOptions.Retry` and `ParallelCycleOptions.Retry` add an optional retry
policy to the common controller. The zero value retains one attempt. Consumers
must select `MaxAttempts`, `Delay`, and an explicit `Retryable` predicate to
enable retries. The limit is four attempts per item per invocation. The delay
must be at least one millisecond, no more than one second, and no greater than
the action timeout. These are runtime safety bounds, not application capacity
limits.

Every attempt receives a fresh context and a full action time budget. The prior
callback must return before another attempt can start. Its context is canceled
before the delay. The remaining work budget must cover both the delay and the
next action. If it cannot, the action keeps its unresolved error. Cancellation
stops the delay. A terminal peer failure cancels pending parallel retries.
Retries retain the resource key, so equal keys remain serial within a page.
They add no cross-process fencing.

The predicate must return promptly and be safe for concurrent calls. It must
permit repetition of the complete action after an uncertain result. The
runtime never retries its own scan contract or source-window errors. It does
not retry source reads or conditional checkpoint saves. It does not store or
log provider error text. Runtime retry policy does not replace application
authorization or provider ownership checks.

A successful retry resolves only the current action failure. It cannot clear
a failure already stored in the cycle checkpoint. Exhausted attempts retain
the existing failure, cursor, conditional-save, and restart rules. The existing
checkpoint format is unchanged. The existing successful full scan and provider
inventory checks remain necessary before an application reports cleanup.

The generated tests cover both telemetry modes, serial and parallel use,
default behavior, error selection, attempt limits, complete time reserves,
parent and peer cancellation, key ordering, saved failure retention, and save
conflicts. Candidate `d205073` passed all six full compiler jobs and 34 race
packages. The focused check passed 50 named retry cases in each telemetry mode.
Exact main source `ee348b8` also passed all six jobs and 34 race packages in
run `35474193099`. Focused run `35474193209` passed 17 outer cases and 50 named
retry cases in each telemetry mode. Two isolated builds produced identical
compiler bytes. Independent checks matched 1,287 source files, 28 dependency
records, the build policy, and both signatures. The local and release installers
verified the same package. No downloaded compiler ran on the workstation.

The [signed compiler release](https://github.com/jsell-rh/stego/releases/tag/compiler-ee348b819bb43a738a6aad2fc14cb76ce3b1ec21)
is immutable. See the [complete evidence](controller-cycle-retry-evidence.json).
Consumer regeneration, application checks, and a new capacity measurement remain
required. No application capacity improvement is claimed.
