# Verdict: decode-rate

Environment: server3 (the E-box), 8-core AMD EPYC VPS, Ubuntu 24.04, single-threaded sweeps, 2026-07-18.
Go binary cross-compiled locally (go 1.x, GOAMD64 v1 unless a row says v3); C reference gcc 13.3 -O3 -march=native compiled on the box.
Three runs: the full sweep twice (repeatability within ~10%, ordinary VPS noise) and an unpack-only GOAMD64=v3 run.
Rates below are from the first run unless noted; postings rates are M postings/s single core, GB/s is decoded output bytes.

## Predictions scored

- P1 (pure-Go unpack 1.5-2.5 GB/s decoded per core at production widths): FAIL. At block 128, w4-w12 lands at 0.61-1.04 GB/s (260 down to 153 M/s), roughly half the predicted floor. The same binary runs 3-5x faster on the dev laptop, so part of the miss is this EPYC VPS's memory subsystem, but E-real means these are the numbers that gate.
- P2 (Go within 2.5x of the same-box C reference): FAIL. C-to-Go ratios at block 128: w3 2.11, w4 2.50, w5 2.33, w6 2.79, w7 2.65, w8 2.63, w10 2.93, w12 3.32, w16 3.41. The production-mass widths past w5 sit outside 2.5x. GOAMD64=v3 closes 5-15% (w12 153 to 162, w16 124 to 145 M/s), not the gap. The K2 clause is now armed: if C1b's Q1 budget needs the difference, a faster kernel (assembly or a batched rewrite) is on the table, and the C rows prove the format itself supports 2.2-2.8 GB/s.
- P3 (block 128 on the flat part, 64 pays visible header overhead): HALF. Flat is confirmed harder than predicted: all three block sizes land within ~11% on every tier (dense 81.0/78.1/70.1 M/s at 64/128/256). But the direction is monotone down, and block 64 shows no header penalty at all on this box. Block 128 stays: nothing in the rate curve argues for moving, and the impact-resolution argument that caps block size is untouched. Doc 05's flat-part sentence should say flat-to-slightly-declining.
- P4 (FOR unpack beats vbyte by 2x up to w8): FAIL, and this is the surprising row. Vbyte matches or beats the bitpack unpack across the mid widths: w5 vbyte 311 vs unpack 254 M/s, w7 292 vs 208, w8 is the one vbyte cliff (107, two-byte varints with an unpredictable length branch). Unpack only wins clearly at w10+ and marginally at w1-w3. The scalar shift-mask loop, not the format, is the Go-side bottleneck; the C reference runs the identical algorithm 2.5x faster.
- P5 (fused tombstone probe costs 10-40%, beats or matches second pass): MARGINAL. Fused costs ~45% (80.8 to 44.1 M/s) against decode-only, just past the band, because every probe misses to DRAM in a shard-sized 64 MiB set. Fused and second-pass are statistically equal across the three runs (44.1/47.9, 45.2/44.0, 45.6/35.7); call it a tie and keep the fused shape for its simplicity.
- P6 (hotfmt within 1.2x of the lab block arm): HALF, in the conservative direction. Production is faster than the lab copy, up to 1.26x on dense (98.2 vs 78.1 M/s); the lab arm pays per-block offs/ns/prevs slice loads and 256-capable staging that production's fixed-length path avoids. The lab underestimates production, never overstates it, and the hot rows are the production numbers.

## The Q1 framing

hotfmt.DecodePostingsBlock sustains 72-98 M postings/s per core on this box (mid tier 85 M/s).
At the 10 ms every-query deadline that is ~850k postings per core per query, ~6.8M across 8 cores, before scoring, skips, or the block-max pruning that exists precisely to avoid decoding most blocks.
Whether that is enough is C1b's Q1 lab's question; if it is not, the C rows say a 2.5x faster kernel is there to be taken.

## Findings beyond the predictions

- The vbyte result reframes the codec choice: the FOR blocks' win on this box is space (GBin columns) and patch-free wide gaps, not raw decode speed. Any kernel work should target the shift-mask loop first; it leaves 2.5x on the table against gcc on identical bytes.
- GOAMD64=v3 is nearly free to adopt for server builds (5-15% on unpack rows, nothing on the block-shaped arms) but is not a substitute for kernel work.
- Absolute rates on this VPS are 3-5x below the dev laptop across both languages; per-box calibration rows (the cref arm) must ride along in any future decode lab so cross-box comparisons stay honest.
