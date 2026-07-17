# Chain-rate verdict

Run 2026-07-17 against the prediction in PREDICTION.md, which merged before any server run (PR #21, chizu commit 35c8fea).

## Environment

- MinIO RELEASE.2025-09-07T16-13-09Z as a bare pinned binary on server3 (8 cores, 24 GB, x86_64), data on the local disk.
- Cross-box arm: driver on server2 (6 cores, 12 GB), RTT server2 to server3 avg 1.637 ms over 5 pings.
- Localhost arm: driver on server3 against 127.0.0.1.
- Raw sweeps: results/20260717-server2-to-server3.txt and results/20260717-server3-local.txt.

## Predictions scored

- P1 PASS (the gate claim). The design-load arm on server3-hosted MinIO from server2: 180/180 appends landed at 3.0 appends/s carrying 23.7 records/s, 412 re-read rate 0.0%, zero failures. Same shape on localhost: 180/180, 0.0%, zero failures.
- P2 SPLIT. Cross-box single-contender saturation measured 81.8 to 84.4 appends/s, inside the predicted 30-150. Localhost measured 93.6 to 106.3 against a predicted 150-500: MISS low. The 150-500 prior came from laptop NVMe runs; on server3 the append round trip is storage-bound, not network-bound. Localhost p50 is 9 ms against 11 ms cross-box, so the 1.6 ms network hop is a rounding error on MinIO's roughly 9 ms per conditional PUT on this disk. The ceiling per sequential writer on this fleet is about 100 appends/s wherever the driver sits.
- P3 PASS. Total throughput at 8 saturated contenders stayed within 2x of 1 contender in both arms: cross-box 47.5-53.7 vs 81.8-84.4 appends/s, localhost 72.8-77.5 vs 93.6-106.3. Every 412 means someone else landed a slot; global forward progress never stalled and no arm recorded a single failure.
- P4 PASS. Batching is nearly free: at 1 contender, 32-record batches ran at 101% (cross-box) and 96% (localhost) of the 1-record appends/s, so records/s scales linearly with batch size, topping at 3267 records/s localhost and 2642 cross-box.
- P5 PASS. Design-load p99 was 51 ms cross-box and 23 ms localhost, both far under the 250 ms bound.

## Doc 02 section 4 margin arithmetic, confirmed with measured numbers

The design coordination load at 100B scale is about 20 records/s.
Measured on the fleet: the paced design arm carries 24 records/s in 3 appends/s at a 0% 412 rate, and saturated capacity is about 80 appends/s cross-box, 2400 to 3300 records/s at 32-record batches.
That is roughly 25x headroom in appends and over 100x in records, so the design load sits deep inside the margin with batching, as the milestone box asks.

## Findings beyond the predictions

- Tail starvation under saturation. The contender that just won a slot PUTs again immediately while every loser pays a GET first, so it keeps winning; per-contender spreads at 8 contenders ran from 16 to 897 appends, and the worst single append stalled 28.5 s in read-lose cycles. Max latency in every contended saturated arm sat in the 5 to 28 s band. Throughput and safety are unaffected, and paced traffic never sees it, but any future component that saturates the chain wants jittered backoff after consecutive losses.
- The 412 metric needs the poll-then-append discipline to mean anything: without polling between paced appends, every append structurally eats contenders-1 re-reads just catching up (178% measured in the local smoke), which is chain reading mislabeled as contention. The lab polls to the tail before each paced append, like a real node.
- The live-S3 arm has not run: no AWS credentials exist on this machine and the Cloudflare wrangler token is expired, so no real-cloud bucket was reachable. The PRED pass rule gates on server3-hosted MinIO, which passed; the live-S3 run stays open as a follow-up when credentials exist.

## Verdict

PRED-CHIZU-C0A-CHAIN passes on server3-hosted MinIO.
The chain sustains the full design record mix in 3 batched appends/s with 0% 412 re-reads at the gate-fleet contender count, and the batch-size lever delivers the doc 02 margin with two orders of magnitude to spare.
