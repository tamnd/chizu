# Skip-depth verdict

Rows: sweep1.tsv (original model, no pruning, no pread floors) and sweep2.tsv (Amendment 1 model), both deterministic counting simulations, seed 2107, 400 queries per config.
Host-independent counts, no perf claim; sweep 2 ran after Amendment 1 merged to main (PR #50).

## Outcomes against PRED-CHIZU-C1B-SKIP

Original predictions, judged on sweep 1:

- P1 MISS: l1l2 knob 1000 cut nothing at rand/3 p99 (899 KB vs 899 KB). The driver's candidates probe nearly every block of the other terms, so touched L1 entries coalesce into the whole array.
- P2 MISS, model scope: 82.6 MB p99 total NVMe against the 3 MB claim, but doc 05's arithmetic says "under MaxScore-style bounds" and sweep 1 modeled no bounds.
- P3 PASS: knob 1000 identical to knob 10000 on every row; knob 0 costs a little more of both. 1000 is safe whenever L2 residency exists at all.
- P4 MISS: the stop class did not blow up l1only relative to rand, because the rand tail contains rank-1 terms with bigger arrays than the stop class average.

Amendment 1 predictions, judged on sweep 2:

- P5 PASS: at survive 1, l1l2 beats l1only nowhere and hybrid tracks l1only.
- P6 MISS, the deciding row: at 500M docs, survive 0.01, rand/3, hybrid p99 skip bytes equal l1only (44,968 KB vs 44,968 KB; p50 8,208 vs 11,984, a 1.46x, not the predicted 3x).
- P7 PASS on bytes (hybrid never exceeds l1only), with an honest caveat: hybrid minimizes bytes and pays for it in pread count (p99 2,402 preads vs l1only's 28 at 500M rand/3). Byte-minimization is the wrong planner objective; a real choice needs a time model. Moot given P6.

## Why the two-level design cannot win here, structurally

An L1 entry is 15 bytes and a pread floor is 4 KiB, so a span read only fragments away from the full array when touched entries sit more than ~273 entries apart.
Touched density is (driver df / term blocks) times span survival, and span survival through a 32-block L2 fan is 1-(1-s)^32, which is still 27.5% at block survival s = 0.01.
So the byte win exists only when span survival drops below ~0.4%, i.e. block survival below ~0.01%.
Whether real MaxScore traversal reaches that on real fixtures is exactly lab 04's measurement, not this lab's model.

## Decisions (per the amended pass rule)

1. L2 leaves the mlock set. The residency plan should mlock head-term L1 arrays instead: top-1000 df ranks total ~10 MB of L1 at 10M docs and ~100 MB at 100M (sum of nb x 15 over the Zipf head), which is bounded and kills the descent question for exactly the terms whose arrays are too big to pread casually. Tail terms carry one-page L1 arrays; read them whole in one pread.
2. The read-planner slice bakes the trivial discipline: full-array L1 reads (resident for the head, one small pread for the tail), no span planning, no L2 descent. The L2 band stays format-reserved (it is derivable from df and costs ~3% of L1), and the decision reopens only if lab 04 measures span survival below 0.4% on real traversal.
3. Warning forwarded to PRED-CHIZU-C1B-Q1: doc 05 section 6's 1-3 MB worst-case traversal arithmetic does not survive this model even at 1% block survival (679 MB p99 at 500M rand/3; 13.6 MB at 10M). The bound Q1 leans on must come from the governor's block budget and measured lab 04 survival rates, not from the section 6 paper numbers. The doc 05 update lands with the lab 04 verdict, when the measured survival number exists to write into it.

## Milestone box

The lab box ticks on this verdict: the superskip knob question is settled by removal (no L2 residency to knob), and the section 6 arithmetic is flagged for re-derivation at lab 04.
