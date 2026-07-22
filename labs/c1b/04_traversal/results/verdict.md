# Lab 04 traversal: verdict against PRED-CHIZU-C1B-TRAV

Rows: results/sweep.tsv, label laptop-counts.
Deterministic counting rows, host-independent, no perf claim; seed
2107, 200 queries per config, K=1000, docs 1M and 10M; the 10M rows
decide, per the prediction's sweep plan.

## Scorecard

P1 MISS. The ratio maxscore/bmw of scored_p50 does not fall from
qlen 2 to qlen 5; in head it rises (1.27 at q2 to 1.84 at q5) and in
torso and tail it is flat at 1.0. In the counted currencies BMW
dominates postings scored at every term count and its edge grows with
terms. The doc 07 section 3 crossover (MaxScore 3+, BMW 1-2) is not
about decode work at all: it lives in the bound checks this lab
prices at zero. At head q5, bmw makes 491k checks p50 against
maxscore's 56k, a 9x gap that widens with qlen, and every bmw
iteration re-sorts its cursors. The crossover is real but its
currency is check/sort CPU, not blocks or postings.

P2 PASS. safe=1 on all 78 rows at both scales; every pruned top-K
matched the exhaustive score sequence exactly.

P3 PASS, the skip-depth follow-up. Min survive_pct across bmw rows at
10M docs is 0.8 (torso and stop), max 1.3 (head q2); all rows sit
80-130x above the 0.01% L2 reopen threshold. The skip-depth decisions
stand and are now measured, not modeled: L2 stays out of the mlock
set, residency mlocks head-term L1 arrays, the read planner bakes
full-array L1 reads. This also confirms the Q1 warning: at ~1% block
survival, 32-block span survival is 27.5%, so the doc 05 section 6
worst-case arithmetic fails exactly as the skip-depth verdict said;
the doc 05 update lands with this verdict.

P4 MISS. Torso pruning is inert at K=1000: maxscore rows equal
exhaustive exactly (the partition never activates because theta stays
below every torso term bound) and bmw saves under 1% of scored. The
miss is benign for the budget: torso queries cost at most 262 blocks
and 33k postings at p99, cheap in absolute terms. The finding to
carry: at K=1000, pruning only engages where bounds spread widely
(head, stop classes), and those are exactly the expensive classes.

P5 MISS on the letter, favorably. bmw decodes 99.9% of exhaustive
blocks on the stop class as predicted, but maxscore decodes 78.2%
p50, under the 80% floor the prediction set; maxscore prunes slightly
more than the floor allowed. The substance holds: the all-stopword
class stays near the exhaustive ceiling for both algorithms.

## Numbers forwarded downstream

- Traversal slice: hybrid choice confirmed in lab currencies. BMW for
  1-2 terms (head q2: 621 blocks p50 vs maxscore's 749, checks still
  cheap with two cursors); MaxScore for 3+ terms (comparable blocks,
  an order fewer bound checks, no per-iteration sort). This re-derives
  the doc 07 section 3 hint with the mechanism named.
- Lab 05 governor: the stop-class p99 decode under maxscore is 38,840
  blocks at 10M docs, ~52 MB of postings at 1,331 B/block, and it
  scales with docs; the block budget, not the traversal, owns this.
- Skip-depth loop closed: measured block survival 0.8-1.3%, reopen
  threshold 0.01% not approached; no L2 reopening.
