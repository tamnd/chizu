# Importer lab verdict

Rows: fetch.tsv and transform.tsv, server3.
Fetch arm: 8 WET files from data.commoncrawl.org (CC-MAIN-2026-25), 486.7 MB compressed, 4 connections.
Transform arm: 2 of those files in RAM (40,624 pages, 312.7 MB text), cumulative stages, 3 timed reps, one core.
Contamination disclosure: an arctic-duckdb publish held ~2 cores throughout (surrounding stamps show load 16.95-21.68 at the importer stanza).
CPU stage numbers are pessimistic by some contended margin; the P5 miss below is far too large for that margin to matter.

## Outcomes against PRED-CHIZU-C2-IMPORTLAB

- P1 PASS. 63.3 MB/s aggregate at 4 connections (bar 60), on a contended box, first attempt. At the measured ~24 B/page compressed (486.7 MB for ~165k pages) that supplies ~2.6 GB/hour/connection-set; network is not the wall.
- P2 PASS. Parse (gunzip + WARC walk) at 105.7 µs/page against the 150 ceiling.
- P3 MISS as measured, likely noise. Canon delta is 8.5 µs/page against the 5 bar; the laptop smoke measured this delta at ~0, and a cumulative-stage delta under load carries the difference of two noisy runs. Re-judged at the encode-fix re-measure before any doc 03 correction is written.
- P4 MISS, real. Hash delta is 147.8 µs/page against the 30 bar. This is already the branchless simhash (4x better than naive); the vote loop over ~1,100 tokens/page at real CC page sizes (7.7 KiB, not the 5 KiB the budget assumed) is the cost, not sha256. Contention does not explain 5x. Either the doc 03 hash line gets revised for real page sizes or the simhash gets an engineering pass (pack 4 lanes per int64, or hash a token sample); decided in the encode-fix slice since both stages re-measure together.
- P5 MISS, the gating row. Full pipeline including cold encode: 826 pages/s on one core against the 5k floor. The stage table names the hot stage unambiguously: encode delta is 949.4 µs/page, 78% of the total 1211.4, versus parse 105.7, hash 147.8, canon 8.5.
- P6 PASS, derived from measured rates only. Source GETs: ~20.3k pages/WET file measured, so 1B pages is ~49k GETs from a free public bucket. Stores: ~2.7 KB/page text (zstddict verdict, same corpus) puts 1B pages at single-digit TB and ~11k PUTs at 512 MiB segments, dollars not hundreds. Compute even at the MISSED rate: 826 pages/s/core x 7 cores is ~5.8k pages/s/node, ~48 hours/billion on one server3-class box, well under the $1,500/month CG3 ceiling on any rental math. The bill was never the risk; the floor is.

## Decision (the pass rule, applied)

P5 missed, so no bootstrap schedule is written and the stage table's verdict binds: encode must be fixed before the scale-out slice bakes worker counts.
Importer core (#64) landed before this bound run on the spec-constants reading (implementation note 064); the pass rule's landing freeze therefore transfers to scale-out, which does not land until encode is fixed or the doc 03 budget revision is written.

The fix has an obvious first lever handed over by the zstddict verdict from the same corpus: dictionary training leaves the cold write path (0.09% gain, trainer crashes on real corpus), and Seal currently pays for it on every segment.
The encode-fix slice drops training, profiles what remains of the 949 µs (zstd level 3 on 7.7 KiB/page accounts for only ~110-140 µs at the measured 51-70 MB/s/core encode rate, so most of the delta is NOT zstd and needs the profile), re-measures the full stage table on server3, and only then does scale-out size its workers.

## Addendum: the encode fix, measured (2026-07-24)

A CPU profile of the transform on one real WET file attributed 40.8% of samples to dict.buildDict: Seal trained a 64 KiB dictionary per segment that the zstddict verdict from the same corpus had just priced at 0.09%.
Training left the Seal path (both segment families; the dict slot stays at zero length and readers keep serving dictionaried segments), and the lab pinned GOMAXPROCS(1) because klauspost's EncodeAll goes GOMAXPROCS-wide, so the old encode row quietly borrowed idle cores and understated true one-core cost.

Re-measured on server3, near-quiet (transform-fix.tsv, header load 3.65), cumulative one pinned core:
parse 98.9, canon 111.5, hash 242.1, encode 420.5 µs/page; 2378 pages/s/core.
The encode delta fell 949.4 to 178.4 µs/page, and since the old row was un-pinned, the honest improvement is larger than the 5.3x the two numbers suggest.
Canon's delta reads 12.6 here against the 5 bar; with the laptop smoke at ~0 this stays judged as cumulative-run noise, not a stage cost.

The simhash vote loop then packed four 16-bit lanes per word (crawl and lab copies identical, equivalence tested); laptop mechanics show 1.77x on the whole function, no perf claim.
The quiet re-measure for the packed hash did not land: the box's quiet window closed mid-chain (transform-swar.tsv header records load 13.77, an sqlo1 corpus run plus ccrawl took the cores) and every delta in that row is contention-dominated. The row is kept for the header discipline and the packed-hash stage table re-measures in the next quiet window.

P5 re-judged: 2378 pages/s on one core against the 5k floor is still short, and the remainder is structural, not fixable in code.
Parse alone is ~99 µs/page of gunzip on real WET input; a 200 µs/page single-core total was never reachable on this box.
CR4's floor is per NODE: three transform cores clear 7.1k pages/s and server3 has eight.
The pass rule's alternative branch fires: the doc 03 budget table gets revised to measured real-page-size lines (7.7 KiB/page raw, not 5), the per-core framing becomes per-node arithmetic, and scale-out unfreezes against the revised budget.
