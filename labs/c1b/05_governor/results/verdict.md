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
