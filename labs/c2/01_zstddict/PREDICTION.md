# PRED-CHIZU-C2-DICT

Filed before the first run, per doc 10 (CZ24).

## Question

Do per-segment zstd dictionaries earn the comp-2 default for the text and outlinks columns at doc 04 block sizes, and does the ~3 KiB/page stored-text constant that every capacity table multiplies survive real Common Crawl text?

## Setup under prediction

Real corpus: WET (extracted text) and WAT (link metadata) files from the latest 2026 Common Crawl monthly, prepped into record files by this lab's prep mode; no fixture text anywhere.
Blocks are built the way coldfmt builds them: len-prefixed cells concatenated to BlockRawTarget (16 MiB raw), one zstd frame per block.
Dictionary arms: none, trained (dict.BuildZstdDict, 64 KiB, the coldfmt parameters), raw (64 KiB stride-sampled content, the hotfmt doc-band mechanism).
Both dictionaries train on the text column and serve both columns, because that is exactly what coldfmt's trainDict does in production.
Levels 1 and 3 (SpeedFastest, SpeedDefault), encoder and decoder pinned to concurrency 1 so rates are per-core.
Perf rows (encode/decode MB/s) bind on server3 (E-box, per the doc 10 lab table); ratio rows are host-independent.

## Predictions

Priors: doc 04 section 3 says trained dictionaries turn ~5 KiB extracted text into the ~3 KiB stored constant; doc 04 size accounting says outlinks store ~600 B/row.
Against that stands the window argument: a zstd dictionary sits before position 0 of the frame and is only reachable from positions inside the match window, so a 16 MiB block already carries its own cross-page redundancy and the marginal gain of any 64 KiB prefix should be small.
The 5-to-3 KiB folklore comes from per-record compression, where dictionaries are the whole game; per-block compression is a different regime and the prediction says so honestly.

- P1 (the 3 KiB constant): text at level 3, best dictionary arm, lands at or under 3.9 KiB stored per raw 5 KiB of text (the doc 09 constant at +30%); reported as stored-bytes-per-page at the corpus's real mean page size alongside the normalized figure.
- P2 (dictionaries are marginal at block scale): the trained dict improves text stored bytes by less than 5% over no-dict at level 3, and the raw dict by less than the trained one.
- P3 (outlinks): outlinks store within 50% of the 600 B/row accounting at level 3, and the text-trained dict moves outlinks by less than 5% too (16-byte urlfps are incompressible noise to a text dictionary).
- P4 (decode stays cheap): level-3 text decode runs at 400 MB/s/core or better on server3, and the dict arms decode within 10% of no-dict, so build passes stay CPU-comfortable against the doc 06 decomposition.
- P5 (level default): level 3 stores at least 8% fewer text bytes than level 1 at no worse than 2.5x the encode cost; 3 stays the store default.

## Pass rule

P1 sets the doc 09 text constant from measurement (whatever the number is, doc 00/09 get the real bytes-per-page).
P2 and P3 decide the dictionary default: if the best dictionary arm gains less than 5% on both columns and P4's decode parity holds, doc 04's comp-2 columns flip to plain zstd (comp 1), per-segment training leaves the cold write path, and the format keeps the comp-2 byte reserved; a gain of 5% or more on either column keeps the dictionary and picks trained versus raw by the gain-versus-B2-complexity trade.
P4 misses reopen the doc 06 build-pass arithmetic before CG3 arithmetic is allowed to lean on it.
P5 misses re-base the level default in doc 04.
