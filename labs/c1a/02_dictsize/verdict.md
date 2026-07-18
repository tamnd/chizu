# Verdict: dict-size

Host: server3 (8c/24GB AMD EPYC VPS, the E-box).
Runs: 2026-07-18, 4M-term fixture vocabulary, seed 2107; one full run plus two extra lookup reps (results/2026-07-18-server3*.txt).
Prediction: PRED-CHIZU-C1A-DICT, merged before the run (PR #35).

## Scorecard

P1 PASS (the gate).
Front-coded structure at block 64 is 6.86 B/term (26.3 MB terms + 1.17 MB index over 4M terms) against a DAFSA floor of 10.35 B/term: 0.66x of the floor, and the floor undercounts any real FST because per-term outputs are not in it.
Both sides landed inside the predicted bands (fc64 6-8, DAFSA 10-14).
Front coding stays; nobody builds an FST for chizu.

P2 PASS.
Lookup falls with block size in every rep: block 32 at 0.100-0.110 M/s, block 64 at 0.061-0.080, block 128 at 0.029-0.045, all single-core.
The 128:32 ratio spans 0.26-0.43, inside the 0.25-0.5 band, and 64 sits between the other two in every rep.

P3 PASS.
Index bytes: 2,340,428 at 32, 1,169,305 at 64, 584,475 at 128; each doubling cuts the index by 0.4997x and 0.4999x, inside the 0.4-0.6 band and closer to exactly half than predicted (first-term string bytes are a smaller share than expected).

P4 PASS, barely, and noise-dominated.
The hotfmt production anchor at 64 ran 0.058-0.068 M/s against the lab's 0.061-0.080; per-rep ratios 0.91x, 1.18x, 1.17x, all inside 1.2x but the rep-to-rep spread on this VPS (~25% on the 64 rows) is as large as the codec gap.
The byte-equality test is the real drift guard; the anchor row confirms no gross divergence.

P5 PASS.
Total dictionary at block 64 is 38.86 B/term (predicted 37-41): 6.86 structure + 32.00 entries.
At the doc 05 shard vocabulary of 300M terms that is 11.7 GB, not the "~8 GB front-coded" in doc 05 section 4; the entry band alone is 9.6 GB.
Doc 05 gets amended to 39 B/term measured, ~11.7 GB per shard dictionary, index ~175 MB RAM at block 64.

## What this settles

Block 64 stays the default.
The size case for 128 is weak (0.20 B/term structure saved, index RAM 584 KB vs 1.17 MB per 4M terms, ~88 MB saved per shard) while lookup drops ~40%; the case for 32 buys ~1.5x lookup for +0.39 B/term and double the index RAM, and dictionary lookups are off the critical path anyway: at 60-80k lookups/s/core a 10-term query spends ~150 us in the dictionary against the 10ms deadline.

Absolute lookup rate on server3 is 61-80k/s/core at block 64 with production's allocation shape (bytes.Clone per scanned term).
If a future profile shows dictionary time mattering, the lever is scan-without-clone, not block size.

The M1 dictionary line was understated in doc 05: measured 38.86 B/term means ~11.7 GB per 500M-doc shard at 300M terms, still comfortably inside the mlock budget next to the ~800 GB postings file, but the number in the doc was wrong and is now fixed.

## Findings beyond the claims

The DAFSA automatizes the fixture vocabulary poorly because ids and numbers share little suffix structure: 4.82M states and 7.30M arcs for 4M terms, 1.8 arcs per term at 2+ bytes per arc.
Front coding beats the automaton on raw structure bytes at every block size measured, before an FST's output bytes are even counted.

Term-area bytes barely move with block size (6.66 vs 7.25 B/term across 32 to 128): the first-term-per-block full copy is the only difference, so almost all the sharing is captured at 32 already.
Block size is purely an index-RAM vs scan-length trade here.

server3's lookup rows run ~5x below the laptop smoke, consistent with the decode-rate lab's 3-5x observation for this VPS class.
