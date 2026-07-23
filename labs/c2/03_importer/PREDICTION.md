# PRED-CHIZU-C2-IMPORTLAB

Filed before the first bound run.
Gates the bootstrap schedule (doc 03 section 13) against the doc 09 CG3 line (import a billion pages for <= $1,500/month all-in) and the CR4 floor (>= 5k pages/s/node with provenance intact).

The lab has two measured arms and one derived table:

- fetch: concurrent GETs of WET files from the public bucket (data.commoncrawl.org over HTTPS), pure network, bytes discarded or kept for the transform arm.
- transform: the per-core CPU decomposition of the import path, cumulative stages parse -> canon -> hash -> encode, held against the doc 03 CPU budget table.
- cost: the $/billion arithmetic in the verdict, parameterized ONLY on measured bytes/page and stage rates, never on paper constants.

## Predictions

- P1 (network is not the wall): 4 concurrent connections sustain >= 60 MB/s aggregate on server3. Compressed WET runs ~3 KiB/page, so 60 MB/s supplies ~20k pages/s, 4x the CR4 node floor before a single worker thread is added.
- P2 (parse): gunzip + WARC walk of WET conversion records costs <= 150 us/page/core on server3, the doc 03 "decode+parse" line. WET is pre-extracted text, so this should land well under; the line is the ceiling, not the estimate.
- P3 (canon): canonicalizing the page URL and computing its fingerprint adds <= 5 us/page. The doc 03 budget gives 50 us for ~50 outlink canonicalizations at crawl time; the WET import path canonicalizes one URL per page, so the per-URL cost implied here must stay ~1 us or the crawl-time budget is broken too.
- P4 (hash): sha256 over the text plus a 64-bit token simhash adds <= 30 us/page, the doc 03 line.
- P5 (the floor, the one that gates): the full pipeline including cold page-segment encode (Add + Seal, dictionary training and zstd included) sustains >= 5k pages/s on ONE core of server3. Then a node meets CR4 on a single core and the 8-core E-box has >= 8x headroom for fetch overlap, dedup, and frontier writes.
- P6 (the bill): stored bytes/page measured here times PUT count at 512 MiB segments, plus GET count for the source files, lands the derived $/billion at <= $1,500, the CG3 ceiling (doc 09's own arithmetic says ~$1,230).

## Pass rule

P1 and P5 both hold on server3: the bootstrap schedule is written into the verdict (1B pages in days on the standing 3-box fleet, from measured fetch MB/s and transform pages/s/core), and importer core bakes its worker counts from the stage table.
P5 misses: the stage table names the hot stage and importer core does not land until the stage is either fixed or the budget revision is written into doc 03.
P2-P4 individually missing while P5 holds is a budget-table correction, not a gate failure; the correction goes to doc 03.

## Bound

Laptop runs are smoke only, no perf claims; every verdict row names the host.
The fetch arm depends on Common Crawl's CDN and the box's uplink; a P1 miss on one run is re-tried in a different hour before it counts, because the bottleneck under test is our uplink, not their edge.
