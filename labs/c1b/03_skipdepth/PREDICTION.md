# PRED-CHIZU-C1B-SKIP

Filed before the first run, per doc 10 (CZ24).

## Question

Does the two-level skip design (doc 05 section 6) earn its L2 band, what should the superskip residency knob default to, and does the section 6 worst-case traversal arithmetic (~1-3 MB NVMe per 3-term query) survive a Zipf-realistic simulation?

## Setup under prediction

Deterministic counting simulation, no time dimension: rows are a pure function of the seed and identical on every host, so this lab carries no perf claim and the verdict can be written from any machine.
10M simulated docs (the C1a fixture scale), Zipf docfreq with a 45% head term, query terms drawn Zipf over a 1M vocabulary plus an adversarial all-stopword class (ranks 1..50).
The rarest term drives and reads everything of its own; other terms are probed at every driver candidate with no pruning model, so every row is the worst case the governor budgets against.
Arms: l1only (a probed term preads its whole L1 array) versus l1l2 with residency knob 0 / 1000 / 10000 (top-knob df ranks keep L2 mlocked; colder terms pay one extra L2 pread).

## Predictions

Priors: section 6 says a df-500M term carries 39 MB of L1, and scaled to a 10M shard the head term still carries ~660 KB of L1 that l1only re-reads on every query it appears in; touched-span reads should collapse that by the ratio of driver df to term blocks.

- P1 (L2 earns its band): at rand class, 3 terms, l1l2 knob 1000 cuts p99 skip-band bytes at least 10x versus l1only.
- P2 (section 6 arithmetic): rand 3-term p99 total NVMe (skip bytes plus probed blocks at ~1.3 KB) lands at or under 3 MB, the top of the section 6 claim, without any pruning credit.
- P3 (knob default): knob 1000 is within 10% of knob 10000 on p99 skip bytes at every class, and knob 0 costs measurably more preads than knob 1000 (each cold term pays an L2 pread); 1000 is the default the residency plan bakes.
- P4 (the Zipf killer): at the stop class, l1only p99 skip bytes run at least 50x its own rand-class p99, while l1l2 knob 1000 stays within 10x of its rand-class p99. This is CZ3's survival mechanism in one row.

## Pass rule

The lab box ticks for the two-level design if P1 holds; P3 sets the knob default the residency-plan slice bakes; P2 misses reopen the section 6 arithmetic before PRED-CHIZU-C1B-Q1 is allowed to bind; P4 misses mean the governor needs a tighter block budget for stopword queries than doc 07 currently assumes.

## Amendment 1 (filed 2026-07-23, before the extended sweep)

Sweep 1 (results/sweep1.tsv, run under the original prediction) missed P1, P2, and P4 and passed P3.
The mechanism the rows exposed: with no pruning, the driver's candidates probe nearly every block of the other terms, touched L1 entries sit closer together than one 4 KiB pread, and span reads coalesce into the whole array, so l1l2 equals l1only (899 KB vs 899 KB at rand/3 p99).
P2's miss is model scope: doc 05's 1-3 MB claim says "under MaxScore-style bounds", and sweep 1 modeled no pruning at all.
The extension adds what the misses demand: a survive dimension (block passes block-max with probability s, its L2 span with 1-(1-s)^32, correlated), 4 KiB pread floors on every skip read, a hybrid arm (the planner picks span-reads or the full array per term, whichever is fewer bytes), and a 500M-doc scale point where a head term carries a ~26 MB L1 array.
Knob is fixed at 1000 per P3.

- P5 (floors kill naive spans): at survive 1, l1l2 beats l1only nowhere; the hybrid arm tracks l1only within one page per term. The no-pruning worst case belongs to the governor, not to this band.
- P6 (scale plus pruning is the win): at 500M docs, survive 0.01, rand 3-term, hybrid p99 skip bytes land at least 3x below l1only.
- P7 (pure upside): hybrid never exceeds l1only anywhere, so the two-level band plus a planner choice costs nothing where it cannot win.

Amended pass rule: the lab box ticks for the two-level design if P6 and P7 hold, with the read-planner slice baking the hybrid choice and knob 1000; if P6 fails too, L2 stays format-reserved but the residency plan drops it from the mlock set and the milestone re-derives.
