# zstd-dict verdict

Rows: sweep.tsv, server3, real Common Crawl corpus (40,624 WET text rows, 40,073 WAT outlink rows), 16 MiB raw blocks built the coldfmt way, encoder and decoder pinned to concurrency 1, 3 reps.
Contamination disclosure: the CPU-rate columns (encMBps, decMBps) ran at the worst load of the whole sweep session (header 51.59/26.96/18.53, an arctic-duckdb publish plus the just-finished governor sweep still draining).
Ratio and bytes-per-page columns are host-independent and bind regardless.

One arm is missing: dict.BuildZstdDict failed on the real CC sample set and the lab skipped the trained arm (logged in run-all.log).
This is the known klauspost trainer crash we hit earlier on synthetic data, now reproduced on real corpus.
The verdict below stands without it, for reasons given, and the upstream issue is held pending the user's go-ahead since filing it is outward-facing.

## Outcomes against PRED-CHIZU-C2-DICT

- P1 PASS with room. Real CC text runs 7.7 KiB/page raw (312.8 MB over 40,624 pages), well above the 5 KiB planning figure. At level 3 it stores at 2719 B/page, ratio 0.337; normalized to the doc 09 framing that is ~1.7 KiB stored per 5 KiB raw, far under the 3.9 KiB bar. The capacity-table constant becomes: raw text 7.7 KiB/page, stored text ~2.7 KiB/page, which lands UNDER the 3 KiB stored constant even though raw is 54% above plan. Corpus-stats settles it at 1B-page scale.
- P2 PASS on the arm that ran. The raw stride-sampled dict gains 0.09% on text at level 3 (105.4 -> 105.3 MB stored). The window argument the prediction filed is confirmed: a 16 MiB block already carries its cross-page redundancy in-window and a 64 KiB prefix adds nothing. The trained arm could not run, but it was predicted to gain under 5% for the same structural reason, and its trainer crashing on real corpus is itself a write-path liability the production path should not carry.
- P3 SPLIT. The dict-movement clause passes (raw dict moves outlinks 0.02%). The size clause misses hard: outlinks store 2831 B/page at level 3 against the ~600 B/row accounting, on 5.3 KiB/page raw outlink data. Real CC pages carry far more link bytes than the doc 04 accounting assumed. This is a doc 00/04 accounting correction, forwarded to corpus-stats, not a dictionary question.
- P4 MISS as measured, contaminated. Level-3 text decode measured 306 MB/s/core against the 400 bar, on a box at load 51 with the decoder pinned to one of ~5 free cores. Re-measure on quiet before the doc 06 build-pass arithmetic leans on the number. Note the raw-dict arms decode SLOWER than no-dict (149 vs 306 on text L3), one more reason the winning arm is no-dict.
- P5 NARROW MISS, host-independent so it binds. Level 3 stores 7.0% fewer text bytes than level 1 (bar 8%) at 1.4x the encode cost (bar 2.5x). Per the pass rule the level default re-bases in doc 04: the verdict keeps level 3, because 7% of a petabyte-class corpus is real money and 1.4x encode cost on a stage the importer lab just showed needs work anyway is not the binding constraint; the doc records 7%, not the folklore 8%+.

## Decision (the pass rule, applied)

The best available dictionary arm gains under 5% on both columns, so doc 04's text and outlinks columns flip to plain zstd (comp 1), per-segment dictionary training leaves the cold write path, and the comp-2 byte stays reserved in the format.
The decision reopens only if a fixed upstream trainer shows a 5%+ gain on either column, which the window argument makes unlikely.
Immediate beneficiary: the importer's encode stage, whose Seal currently pays for dictionary training the corpus does not reward (see the importer verdict).
