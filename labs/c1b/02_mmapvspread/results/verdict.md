# mmap vs pread verdict

Rows: sweep.tsv, server3 (8-core EPYC VPS, 24 GB RAM, virtio disk), same 48 GiB file as lab 01, drop_caches before every config.
Contamination disclosure: an arctic-duckdb publish held ~2 cores throughout (header load 17.63/15.97/14.44).
Both disciplines ran under the identical antagonist mix, so the relative shape this lab exists for is preserved; absolute latencies carry the usual virtio caveat plus the load caveat.

## Outcomes against PRED-CHIZU-C1B-MMAP

- P1 PASS at 4 and 16 workers (p50 within 2x: 1251 vs 1753, 1343 vs 1433). At 64 workers the quiet mmap row shows p50 3.0µs with majflt at 35% of samples, meaning two thirds of its accesses were already page-cache warm; the quiet arm at high worker counts self-warms faster than drop_caches isolation intended. The 64-worker quiet comparison is therefore not clean, and P1 rests on the 4/16 rows. Neither discipline wins the quiet box, as predicted.
- P2 PASS, the deciding row, decisively. Under the scan antagonist mmap p999 is 45.9ms vs pread 15.6ms at 16 workers (2.9x, bar 2x) and 269ms vs 36ms at 64 workers (7.5x). And the 64-worker mmap row was PARTIALLY WARM (majflt 5048 of 31603 samples): even with two thirds of accesses skipping the device, the faulting third stalls behind reclaim badly enough to blow the tail 7.5x. A warm-biased row that still loses this hard only strengthens the pread verdict.
- P3 PASS on the latency clause: under hog, mmap p999 is 3.6x pread at 16 workers (44.7ms vs 12.5ms) and 8.4x at 64 (227ms vs 27ms), bar 1.5x. The throughput-drop clause does not evaluate cleanly because later arms ran warmer than the quiet arm (both disciplines gained reads_s under antagonists instead of losing), an artifact of shared page-cache state across the sweep order. The clause was explanatory, not deciding.
- P4 SPLIT: pread majflt is ~0 everywhere (clean). mmap 4-worker rows show ~0.9 faults per op (cold as intended); 16 and 64-worker rows show 0.35-0.55 faults per op, so the high-concurrency rows were only partially cold. Per the prediction's own rule the fully-warm failure mode did not occur (all mmap rows fault heavily), so the rows count, with the warm bias noted above working against the verdict they support.

## Decision

pread wins; the read-discipline box ticks for pooled pread.
P2 held with margin on both antagonists at both deciding worker counts, on a box where the warm bias favored mmap.
Doc 05 section 11 stands as written and no mmap arm goes behind a flag.
The mechanism is the predicted one: a faulting op stalls on reclaim decisions the process does not control, and thousands of major faults per config (versus pread's zero) pin it.
No quiet re-run is needed; contention here played the role the antagonists exist to play.
