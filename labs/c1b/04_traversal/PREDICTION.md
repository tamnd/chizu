# PRED-CHIZU-C1B-TRAV

Filed before the sweep runs; the pass rules below are frozen at merge.

Lab 04 races MaxScore against block-max WAND on a synthetic fixture
query log (head, torso, tail, all-stopword classes; 2 to 5 terms) and
counts work in the currencies the shard budget spends: blocks decoded,
postings scored, resident bound checks. Rank safety against the
exhaustive top-K is asserted per query, never assumed. Rows are
deterministic counts, host-independent, no perf claim.

Two numbers matter downstream: the term-count crossover doc 07 section
3 hints at (MaxScore for 3+ terms, BMW for 1-2, the Lucene 2022
consensus), and the measured block survival rate the skip-depth
verdict (labs/c1b/03_skipdepth) named as the missing input to the doc
05 section 6 arithmetic: L2 skip bands reopen only if survival lands
below ~0.01% per block (0.4% per 32-block span).

## Predictions

P1 (crossover direction). BMW's advantage over MaxScore shrinks as the
term count grows: in head, torso, and tail classes, the ratio
maxscore scored_p50 / bmw scored_p50 at qterms=5 is smaller than the
same ratio at qterms=2. Pass: the ratio falls from qlen 2 to qlen 5 in
all three classes.

P2 (rank safety). Every row reports safe=1 for both algorithms at both
scales. Pass: no row with safe=0. A miss here is a lab bug, not a
finding, and blocks the verdict until fixed.

P3 (block survival, the skip-depth follow-up). The survival
measurement is the bmw rows: bmw checks a block bound before every
decode, so its survive_pct is decoded-over-checked directly.
(MaxScore's column is not a survival rate: essential terms decode
without a preceding check, so the ratio can exceed 100 and is read
only qualitatively.) Pass: min survive_pct across all bmw rows at 10M
docs >= 0.1 (percent), an order of magnitude above the 0.01% reopen
threshold. If this passes, the skip-depth decisions
stand as written: L2 stays out of the mlock set and the read planner
bakes full-array L1 reads.

P4 (pruning magnitude where it should work). On torso queries at 10M
docs, both pruned algorithms score at most 60% of exhaustive's
scored_p50 at every qlen. Pass: all 8 torso comparisons hold.

P5 (adversarial floor). On the all-stopword class, pruning is nearly
inert: both pruned algorithms decode at least 80% of exhaustive's
blocks_p50. Pass: both stop-class rows hold at 10M docs. This is the
same shape the skip-depth lab measured and is why the governor, not
the traversal, owns the stopword worst case.

## Sweep plan

Deterministic counting rows, runnable anywhere (no time dimension, no
perf claim): -docs 1000000 and -docs 10000000, -queries 200, -k 1000,
seed 2107. The 10M rows are the deciding ones; the 1M rows show the
scale trend.
