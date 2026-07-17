# PRED-CHIZU-C0A-CHAIN

Filed before the first run, per doc 10 (CZ24).

## Question

Does chizu's full design record mix (~20 records/s at 100B scale, doc 02 section 4) fit in a handful of batched appends/s on the chain, with 412 re-reads staying under 5% at the gate-fleet contender count?

## Setup under prediction

MinIO (RELEASE.2025-09-07T16-13-09Z) on server3, the driver binary on server2, so every append crosses the real network between the two boxes.
A localhost arm on the MinIO host separates network cost from CAS cost.
The design-load arm is 3 contenders (one per fleet box) at pace 1 append/s each, 8 records per append, 60s: 24 records/s landing in 3 appends/s, a stand-in for the whole fleet's coordination traffic.
The saturation sweep is contenders 1/2/4/8 x records 1/8/32, unpaced, 30s each.
A 412 re-read is a foreign batch folded inside one's own Append call, which is one bounced PUT plus the winner's GET.
Paced contenders poll up to the tail before appending, like a real node riding the chain; batches folded during that poll are ordinary reading, not 412 costs.
Saturated contenders skip the poll because the CAS race itself is what that arm measures.

## Predictions

Priors: sequential CAS-append loops against MinIO in the 2064 work ran in the low hundreds of appends/s on localhost, and cross-box RTT adds low single-digit milliseconds per round trip; each contended miss costs one extra GET (the winner read) plus the retried PUT.

- P1 (the gate claim): the design-load arm sustains its 3 appends/s with a 412 re-read rate under 5% and zero failures. At 3 paced writers on a 4096-slot-per-checkpoint chain, collisions need two PUTs inside the same slot's flight time (~5-15 ms cross-box), so most seconds see none.
- P2: single-contender saturated throughput lands at 30-150 appends/s cross-box (RTT-bound), and 150-500 appends/s on localhost.
- P3: under saturation the 412 rate grows with contender count but the chain keeps global forward progress: total appends/s at 8 contenders stays within 2x of the 1-contender number in either direction, because every 412 means someone else landed a slot.
- P4: records-per-append is nearly free: at fixed contenders, 32-record batches move at >= 70% of the appends/s of 1-record batches, so records/s scales roughly linearly with batch size. Batching is the lever that buys the doc 02 margin.
- P5: design-load p99 append latency stays under 250 ms cross-box.

## Pass rule

The PRED box ticks if P1 holds on server3-hosted MinIO.
P2-P5 misses do not fail the gate but must be explained in the verdict.
