# PRED-CHIZU-C1B-MMAP

Filed before the first run, per doc 10 (CZ24).

## Question

Does mmap lose to pooled pread on tail latency for high-fan-out band reads once the box is under load, as doc 05 section 11 asserts, or does the mapping's zero-syscall hot path win enough to overturn the pread default?

## Setup under prediction

Sweeps on server3 (8-core AMD EPYC VPS, 24 GB RAM, the E-box), quiet box only, against the same 48 GiB seeded file as lab 01, /proc/sys/vm/drop_caches before every config.
Both disciplines land 16 KiB from random aligned offsets in a caller buffer; pread via ReadAt, mmap via a copy out of a shared mapping that pays its faults inside the timed op.
Configs cross workers {4, 16, 64} with antagonists: none (quiet), scan (a sequential full-speed reader thrashing the page cache), hog (12 GiB anonymous memory held resident so reclaim pressure is constant).
The majflt column pins the mechanism: cold mmap rows should show about one major fault per op, pread rows about none.
Same virtio caveat as PRED-CHIZU-C1B-PREAD: absolute latencies are directional for production NVMe; the relative shape between the two disciplines on one host is what decides the discipline and it is host-honest.

## Predictions

Priors: a cold 16 KiB access costs the same device read either way, so quiet cold rows should split on overhead only (a fault round-trip is comparable to a pread syscall); the divergence doc 05 predicts appears under reclaim, where a faulting mmap op can stall on page-cache writeback/eviction decisions the process does not control, while pread misses queue at the device like any other I/O.

- P1 (quiet parity): with no antagonist, mmap and pread p50 are within 2x of each other at every worker count. Neither discipline wins the quiet box; the decision cannot come from this arm.
- P2 (the doc 05 claim, the row that decides): under the scan antagonist at 16 and 64 workers, mmap p99.9 is at least 2x pread p99.9. Page-cache thrash turns faults into stalls behind reclaim.
- P3 (pressure): under the hog antagonist, mmap p99.9 is at least 1.5x pread p99.9 at 16+ workers, and mmap throughput (reads_s) drops more from its quiet value than pread drops from its own.
- P4 (mechanism): cold mmap rows show majflt within 2x of samples; pread rows show majflt under 1% of samples. If mmap rows show no major faults the sweep was not cold and no row counts.

## Pass rule

The read-discipline box ticks for pread if P2 holds (P1, P3 explain it; P4 validates the sweep).
If P2 fails and mmap's p99.9 is at or below pread's under both antagonists at all worker counts, doc 05 section 11 gets reopened and the C1b read-path slice gets an mmap arm behind a flag before any constant bakes.
