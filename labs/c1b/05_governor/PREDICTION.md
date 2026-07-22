# PRED-CHIZU-C1B-GOV

Filed before the sweep runs; pass rules frozen at merge.

Lab 05 tests the governor contract of doc 07 section 5 in isolation:
a cooperative deadline governor checked only at read-batch boundaries
(depth-32 concurrent 4 KiB preads plus a decode-shaped pass), with T1
(60%, heap-or-tighten, D-terms), T2 (75%, phase 2 starts, D-blocks),
T3 (95%, serialize, D-k). Block counts per query are drawn lognormal
from the traversal lab's measured per-class distributions at 10M docs
(maxscore, qlen 3); classes head/torso/tail/stop plus a 30/40/25/5
mix. This is a timing lab: the deciding rows run on server3 under
gate-shaped concurrency (workers=8, one per core) and every row names
its host. Budgets 6/8/10/12ms.

## Predictions

P1 (compliance, the contract itself). p100 latency stays within
budget plus one batch of slack at every budget and class on the quiet
gate box: p100 <= budget + 1ms. Pass: all rows. This is the whole
point of checked-at-boundaries over preemption; if a depth-32 batch
under load can blow a millisecond, the check granularity is wrong and
the slice design must change.

P2 (the mix rate, and the honest gap to Q3). On the mix arm at 10ms,
deg_pct lands between 10 and 35, dominated by head-class D-blocks.
Pass: mix row at budget 10 inside that band with dblocks_pct the
largest component. A laptop mechanics run (no perf claim) already
shows head degrading ~54% at 10ms even with cached reads, so this
shaped workload cannot meet the Q3 bar of <1%: it models an uncached
maxscore traversal at qlen 3, no block cache, no BMW-for-short
hybrid, no head-term residency. The distance between this row and 1%
is the pruning-plus-cache work the C1b slices must deliver, measured
here so the slice PRs have a number to close against.

P3 (monotone curve). For every class, deg_pct falls or stays equal as
the budget rises 6 to 8 to 10 to 12ms. Pass: no class row increases.

P4 (the stopword floor becomes a governor fact). The stop class at
6ms degrades on more than 90% of queries, dominated by D-blocks; by
12ms it still degrades on more than half. Pass: both bounds hold.
This is the number the traversal lab forwarded (38,840 blocks p99 at
10M docs): the governor, not the traversal, owns this class.

## Sweep plan

server3 only, quiet box (after the 10M gate run exits), backing file
reused from lab 01 (48 GiB, drop_caches before each budget), budgets
6/8/10/12, workers 8, 2000 queries per row, seed 2107. Rows from any
other host are mechanics-only and carry no verdict weight.
