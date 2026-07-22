# PRED-CHIZU-C1B-PREAD

Filed before the first run, per doc 10 (CZ24).

## Question

Does goroutine-pread reach device queue depth at microsecond-class per-op overhead on the gate box, so the read planner (doc 07 section 3) can hit its latency budget without io_uring (doc 01 section 10), and what batch shape should the planner default to?

## Setup under prediction

Sweeps on server3 (8-core AMD EPYC VPS, the E-box), quiet box only, against a 48 GiB seeded random file (2x RAM) with /proc/sys/vm/drop_caches before every cold config.
Four arms: hot (page-cache-resident 4 KiB preads, device out of the loop, pure syscall plus scheduler cost), depth (cold random preads, blocks 4/16/64 KiB, depths 1..64), batch (B scattered 16 KiB blocks as one concurrent batch through a depth-D pool, B in 8/32/128, D in 8/16/32), and waste (32 needed blocks plus 0/16/32/64 speculative extras at depth 32, latency counted to the last needed block).
One caveat named up front: server3's disk is virtio on a VPS, not the raw i8g-class NVMe of doc 01 section 10, so the device envelope rows (absolute IOPS, depth-1 latency) transfer to production hardware only directionally; the overhead rows (hot arm, waste deltas, scaling shape) are the rows this lab exists for and they are host-honest.

## Predictions

Priors: pread syscall cost is ~1-3µs on current Linux; Go parks a goroutine in a blocking syscall for low single-digit µs of scheduler work; doc 01's envelope says 4 KiB random reads are 60-100µs at depth 1 on real NVMe, likely 100-400µs on a virtio VPS; concurrent blocking preads are the literature's boring-but-fine path when latency is disk-bound.

- P1 (the milestone claim): the hot arm holds per-op p50 ≤ 5µs and p99.9 ≤ 50µs at every depth up to 32. That is microsecond-class overhead; the device dominates by an order of magnitude and io_uring stays a road not taken.
- P2 (depth is real): cold 4 KiB IOPS at depth 32 reaches at least 8x depth 1, and depth 64 does not regress p50 by more than 2x versus depth 32 (the virtio queue may cap earlier than real NVMe; a cap with flat p50 still passes, a p50 blowup fails).
- P3 (the planner budget): a 32-block 16 KiB batch at depth 16 completes cold in p50 ≤ 3ms and p99.9 ≤ 10ms on this disk. On doc 01's real-NVMe envelope the same shape scales to ~1ms-class, which is what Q1's 5ms cold bound spends on I/O; if this row lands far off, the C1b read-planner slice re-derives its batch default before landing.
- P4 (waste is bandwidth, not latency): doubling a 32-needed batch with 32 speculative extras raises time-to-needed p50 by ≤ 25%, because the extras ride the same concurrent window. If this fails the planner's aggressiveness knob defaults conservative.
- P5 (block size): 16 KiB blocks deliver at least 3x the MB/s of 4 KiB at equal depth while p50 stays within 2x, confirming L1-span-sized reads as the planner's unit instead of per-4KiB preads.

## Pass rule

The PRED box ticks if P1 and P2 hold on server3.
P3 calibrates the C1b read-planner slice's defaults and feeds the Q1 provisional arithmetic; P4 sets the aggressiveness default; P5 confirms the read unit.
Misses on P3-P5 do not fail the gate but must be explained in the verdict, and a P1 miss with a Q1 miss downstream is the only combination that reopens the io_uring question.
