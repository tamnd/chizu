# Read-planner sweep verdict

Rows: sweep.tsv, server3 (8-core EPYC VPS, virtio disk), 48 GiB backing file, 2s per config, drop_caches before every cold arm.
Contamination disclosure: the box was NOT quiet.
An arctic-duckdb publish job held ~2 cores for the whole run; the header row records load 9.24/15.37/14.12 at start.
The user ordered the run under contention rather than waiting for a quiet window that never came; every judgment below says which rows that touches.

## Outcomes against PRED-CHIZU-C1B-PREAD

- P1 SPLIT, contaminated on the tail. Hot-arm p50 holds 2.5-3.2µs at every depth through 32, comfortably under the 5µs bar, and flat p50 across depth is exactly the microsecond-class overhead claim. Hot-arm p999 blows the 50µs bar everywhere above depth 1 (67µs at depth 1 rising to 11.7ms at depth 32). A page-cache pread cannot take 11ms; that tail is runqueue wait behind the antagonist, and it grows with depth because more runnable goroutines queue behind two stolen cores. The p50 half of P1 binds; the p999 half needs one quiet 2-minute hot-arm re-run before the PRED box ticks clean. It rides the next quiet moment (the pre-gate-run check).
- P2 PASS. Cold 4 KiB IOPS at depth 32 is 10,097 reads/s against 344 at depth 1, a 29x scaling (bar: 8x). Depth 64 p50 is 3031µs vs 1962µs at depth 32, a 1.54x (bar: under 2x, no blowup). Depth is real on this virtio disk and these rows are device-bound, the least contention-sensitive rows in the sweep.
- P3 MISS, and the planner adapts. The 32-block 16 KiB batch at depth 16 came in at p50 19.6ms / p999 277ms against the 3ms/10ms bar. Some of that is the antagonist, but not 6x of p50: the virtio device simply does not deliver the doc 01 NVMe envelope this bar was derived from. Per the pass rule the read-planner slice re-derives its batch default before landing: cold whole-band batches must be budgeted from the measured depth-16/16KiB line (~1.7ms p50 per block, 97 MB/s aggregate), not from the 1ms-class paper figure.
- P4 PASS at the specified point. 32 needed + 32 speculative extras at depth 32: time-to-needed p50 6364 -> 7922µs, +24.5% against the ≤25% bar. The +64 row costs +84%, so the aggressiveness knob defaults to at most 1x speculative extras.
- P5 PASS. 16 KiB vs 4 KiB at equal depth: 4.8x the MB/s at depth 8 (53 vs 11) and 3.3x at depth 32 (137 vs 41), with p50 within 1.3x. The planner's read unit is the 16 KiB L1 span, confirmed.

## Decisions

1. io_uring stays a road not taken: P2's 29x depth scaling plus P1's flat 3µs p50 say goroutine-pread reaches device queue depth at negligible overhead. Nothing here reopens doc 01 section 10.
2. The read-planner slice (held branch slice/c1b-readplanner) lands with pread + depth-32 pools, 16 KiB units, speculative extras capped at 1x needed, and a batch latency budget derived from this table, not from doc 01 paper numbers.
3. PRED box tick is deferred on the P1 p999 clause alone; the quiet hot-arm re-run is ~2 minutes and rides the next quiet window. Every other row either binds now or is device-bound enough that contention does not change the verdict.

## Addendum: the quiet re-run (2026-07-24, results/quiet.tsv)

The window opened when arctic-duckdb finished; the header records load 3.18 and the hot arm ran first, so its rows are the quiet ones this addendum exists for.

P1 judged, and it misses for real.
The p50 clause passes everywhere: 2.5-3.8µs through depth 32, microsecond-class per-op overhead confirmed.
The p999 clause fails everywhere: 96.3µs at depth 1 against the 50µs bar, rising to 340.9µs at depth 8, 3.05ms at 16 and 9.49ms at 32.
Two mechanisms, both named: at low depth the tail is hypervisor steal on a shared VPS (a page-cache pread has no device in the loop and still spikes to ~100µs about once per thousand ops); past 8 issuing goroutines on 8 cores the tail is runqueue wait, growing roughly linearly with depth beyond cores.
The PRED box stays unticked per the frozen rule (P1 and P2 must both hold), and per the same rule io_uring reopens only if Q1 also misses downstream; nothing reopens today because P2 is emphatic.

P2 re-confirmed quiet and better: cold 4 KiB scales 588 to 20,488 reads/s from depth 1 to 32 (34.8x against the 8x bar), plateau confirmed at depth 64 (20,430; p50 1.68x, inside the 2x clause).
Depth 32 stands as the knee.

P3 still misses quiet: a 32-block 16 KiB batch at depth 16 lands p50 5.56ms, p999 35.6ms against the 3ms/10ms bars.
Better than the contended 19.6ms, still nowhere near the paper NVMe envelope; the decision (no batch constant, governor owns budgets) already reflects this.

P4 flips from pass to miss, and the contended pass was the artifact: quiet base p50 is 4,297µs and +32 extras cost 6,660µs, +55% against the 25% bar (the contended run's slow 6,364µs baseline hid the extras inside its own noise).
The conservative branch fires: MaxSpeculative drops 1 to 0 in serve/planner.go in this PR, and speculation returns only if the governor ever demonstrates a case for it.

P5 holds: 16 KiB delivers 3.4-3.9x the MB/s of 4 KiB at depths 8/16/64 within the p50 clause; the depth-32 row reads 2.2x because 4 KiB saturates its IOPS ceiling exactly there (20.5k at both 32 and 64).
The read unit stands.
