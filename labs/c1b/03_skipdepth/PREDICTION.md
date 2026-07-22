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
