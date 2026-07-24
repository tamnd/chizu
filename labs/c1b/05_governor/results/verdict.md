# Governor sweep verdict

Rows: sweep.tsv, server3, budgets 6/8/10/12ms, workers 8, 2000 queries per row, seed 2107, backing file from lab 01, drop_caches per budget.
Contamination disclosure: this is the one lab of the five whose deciding rows CANNOT bind under load, and the box was loaded.
PRED-CHIZU-C1B-GOV froze "quiet gate box" into P1-P3; the run started at load 15.58 (header) with an arctic-duckdb publish holding ~2 cores, and the 1-minute average hit 51 as the final budget finished (recorded in the next stanza's header).
The rows are published as measured, the load headers say why, and P1-P3 are deferred to a quiet re-run (~2 minutes) that rides the next quiet window.

## Outcomes against PRED-CHIZU-C1B-GOV

- P1 DEFERRED, measured miss explained by the precondition violation. p100 blew the budget+1ms bar everywhere (e.g. 148.9ms p100 on the 6ms stop row). The governor's compliance slack is one read batch, and lab 01, same box, same file, same hour, measured cold depth-32 batches at 30-50ms p999 under this load. A boundary-checked governor cannot beat the tail of the batch it is inside; on a quiet box that batch tail was predicted (and laptop-smoked) at ~1ms. Nothing here says the check granularity is wrong; it says the batch under it took 30-50ms when 6 cores served 8 workers plus a publish job. Binds only on quiet.
- P2 DEFERRED. Mix at 10ms measured deg_pct 75.05 against the predicted 10-35 band, with dblocks_pct 54.75 the largest component as predicted. Inflated read times push more queries over T2/T3 thresholds, so the level is contaminated even though the composition matches the prediction.
- P3 DEFERRED, measured miss with a clean mechanism. Torso/tail/mix all rise at budget 12 after falling 6 through 10 (torso 99.6 -> 93.5 -> 87.4 -> 94.3). The budget-12 rows ran last, into the steepest part of the load ramp. A monotonicity claim cannot be judged when the antagonist is itself non-stationary across the sweep order.
- P4 PASS, robust to load. Stop class at 6ms degrades 100% of queries, dblocks_pct 100; at 12ms still 100%, far over the "more than half" bar. Contention only pushes this row toward degradation it already predicted at >90%, and the 38,840-block p99 the traversal lab forwarded is confirmed as a governor-owned fact: no budget in the sweep range rescues stopword-class queries from D-blocks. The block budget, not the traversal, owns this class.

## Decisions

1. P4 binds now: the governor slice designs D-blocks as the primary degrade bit for the stop class, and doc 07's stop-class handling cites this table.
2. P1-P3 re-run on quiet, ~2 minutes total, before the PRED box ticks and before the governor slice bakes its threshold constants (T1 60 / T2 75 / T3 95 are under test in P1-P2, not just the compliance claim).
3. One structural observation survives contamination: even at the measured worst (6ms, stop, p100 148ms), dterms_pct stayed ~0 while dk_pct and dblocks_pct saturated, i.e. the governor sheds blocks and k long before it sheds terms, which is the intended degradation order. The ordering logic is right; the levels await a quiet box.

## Addendum: the quiet re-run binds P1-P3 (2026-07-24, results/quiet.tsv)

The re-run met the frozen precondition: header load 1.77, drop_caches before each budget, same shapes (budgets 6/8/10/12, workers 8, 2000 queries per row).
The numbers barely moved from the contended run, which is itself the finding: the tails were never contamination.

P1 MISS, the one that changes the slice.
p100 lands 33-133ms against budget+1ms bars of 7-13ms, on a quiet box.
The mechanism is the one lab 01 priced the same day: a single cold 32-block batch on this virtio disk carries p999 of 25-36ms, and a governor checked only at batch boundaries cannot cut an in-flight batch.
p50 behaves (mix 7.0ms at the 6ms budget, roughly budget plus one typical batch), so the boundary checks work when batches behave; the contract fails exactly at the batch tail.
The frozen consequence applies as written: the check granularity is wrong and the slice design must change.
The governor slice adds issue-time admission: before issuing a batch, remaining budget is checked against the measured batch-tail envelope (lab 01's rows), and a batch that cannot land inside the budget degrades now instead of blowing p100 later.

P2 MISS on both clauses: mix at 10ms degrades 78.7% against the 10-35 band, and D-k (76.5%) outranks D-blocks (56.6%) as the largest component.
The gap to Q3's <1% is the pruning-plus-cache work the prediction already assigned to the C1b slices; the number to close against is now 78.7, not a band guess.

P3 MISS: tail (71.7 -> 64.6 -> 67.5 -> 68.8) and mix (78.0 -> 77.2 -> 78.7 -> 78.9) rise past budget 8.
All four budgets sit so deep in the must-degrade regime for this uncached workload that the curve is flat noise, not a slope; the monotone claim gets re-tested after the pruning work moves the operating point.

P4 re-confirmed on quiet: stop degrades 100% at every budget with D-blocks at 100%, dterms ~0 everywhere.
The stopword floor is a governor fact and the degradation order (blocks and k before terms) stands.

The PRED box does not tick (P1-P3 all miss); the lab did its job by pricing the real contract violation and handing the governor slice its design amendment plus the 78.7 baseline.
