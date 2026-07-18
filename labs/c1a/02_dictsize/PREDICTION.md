# PRED-CHIZU-C1A-DICT

Filed before the first server run, per CZ24.
The sweep runs on server3 (the E-box) over the fixture vocabulary at 4M terms, seed 2107.

## Claims

P1. Front-coded structure bytes (term area + block index) at block 64 land within 1.3x of the DAFSA byte floor on the fixture vocabulary, and in fact BELOW 1.0x of it.
The DAFSA is a floor for any real FST: a term dictionary FST carries per-term outputs (offsets, df) on top of the automaton, so beating the floor means beating any deployable FST on structure bytes.
A laptop smoke at 200k terms (byte counts are host-independent) already showed fc64 structure at 7.55 B/term against a DAFSA floor of 13.41 B/term, roughly 0.56x; the id and number tail of a web vocabulary shares suffixes poorly, so the automaton pays 2+ bytes per arc where front coding pays for the lcp only once per term.
At 4M terms both sides gain sharing; predicted fc64 structure 6-8 B/term, DAFSA floor 10-14 B/term.
This is the milestone's headline prediction.

P2. Lookup rate falls as block size grows because the scan dominates the binary search: block 128 lands at 0.25-0.5x the block-32 rate, and block 64 sits between them (laptop smoke at 200k terms showed 0.30x and 0.56x; the server ratio should be similar since the scan is the same code either side).

P3. The RAM-resident block index roughly halves per block-size doubling: index bytes at 128 land at 0.4-0.6x of 64, and 64 at 0.4-0.6x of 32.
Not exactly half because each index entry carries the first term's bytes and longer strides do not change term lengths.

P4. The hotfmt production dictionary at block 64 looks up within 1.2x of the lab copy at 64 (same codec, same allocation shape; any bigger gap means the lab drifted).

P5. Total dictionary bytes per term (terms + index + 32B entries) land at 37-41 B at block 64 (laptop smoke at 200k terms: 39.55 B; the entry band dominates and is fixed, term sharing improves slightly at 4M).
At the doc 05 shard vocabulary of 300M terms that is 10.2-12.6 GB, which means the doc 05 section 4 "~8 GB front-coded" M1 line is understated (the 32B entry band alone is 9.6 GB at 300M terms) and gets amended with the measured figure.

## Pass rule

PASS if P1 holds at 4M terms (the FST-floor claim is the gate for keeping front coding over building a real FST).
P2-P5 score independently and feed the block-size choice and the M1 dictionary line; they do not gate.

## Notes

Byte counts are host-independent; only the lookup rows (P2, P4) are perf claims and those come from server3.
The lab codec is pinned byte-for-byte to hotfmt at block 64 by TestLabDictMatchesHotfmt.
