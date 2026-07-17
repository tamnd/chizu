# PRED-CHIZU-C1A-KERNEL

Filed before the first run, per doc 10 (CZ24).

## Question

Do the pure-Go postings unpack kernels decode fast enough on the gate box that the K2 assembly clause (doc 11) stays unarmed, and does block 128 sit on the flat part of the rate curve the way doc 05 predicts?

## Setup under prediction

Single-threaded sweeps on server3 (8-core AMD EPYC VPS, the E-box), one config at a time, 32 MiB of decoded uint32s per pass so the packed input cannot live in L2.
Five Go arms: the raw unpack kernel (widths 1..32, blocks 64/128/256), a vbyte baseline at block 128, the full production-shape block decode (header, patches, prefix resolve, tf unpack, mask fill) at 64/128/256 over three gap tiers (dense w3, mid w7, sparse w16), hotfmt.DecodePostingsBlock on hotfmt.EncodePostings bytes as the production anchor, and the tombstone bitset probe off/fused/second-pass with 10% deleted over a shard-sized set.
The C reference is ref.c, the same LSB-first unpack loop compiled with gcc -O3 -march=native on server3 itself, so the 2.5x clause compares compiler against compiler on one box.
The lab copies are pinned byte-for-byte to hotfmt at block 128 by kernels_test.go.
Rates are quoted as decoded output GB/s (4 bytes per posting) unless a row says otherwise.

## Predictions

Priors: scalar bit-unpack loops in the literature run 1-4 GB/s decoded per core, SIMD ones 4-15 GB/s; Go usually lands within 1.2-2x of gcc on tight integer loops; a fused branch on an L2-resident bitset costs a few percent, on a DRAM-resident one more.

- P1 (the milestone claim): pure-Go unpack sustains 1.5-2.5 GB/s decoded per core at block 128 across the production-mass widths (w4 through w12).
- P2 (the K2 input): at every width and block 128, the Go kernel holds at least 1/2.5 of the C reference's rate. If this fails and C1b's Q1 budget needs the difference, the K2 clause arms; nothing else in the lab failing arms it.
- P3 (the doc 05 flat part): block 128's full-block decode rate lands within 10% of block 256's on every tier, while block 64 pays a visible per-block overhead (at least 5% slower than 128 on the dense tier, where the 8-byte header is largest relative to payload).
- P4: FOR unpack beats the vbyte baseline by at least 2x in postings/s at every width up to w8; vbyte closes the gap at wide widths (w20 and up) where its byte loop stops branching.
- P5: the tombstone probe fused into the decode loop costs 10-40% postings/s against decode-only, because the shard-sized set (64 MiB at the doc 05 cap) makes most probes DRAM misses; fused still beats or matches the second-pass shape, because the docids are already in registers when the probe fires.
- P6 (the copies are honest): hotfmt.DecodePostingsBlock lands within 1.2x of the lab block decode at 128 on every tier; a bigger gap means the lab is not measuring production and the sweep gets fixed before any verdict is written.

## Pass rule

The PRED box ticks if P1 and P2 hold on server3.
P3 decides whether doc 05's block 128 stays or gets amended; either way the constant becomes measured.
P4-P6 misses do not fail the gate but must be explained in the verdict.
