# Working on chizu

The spec is Spec 2107 in the private notes tree (~/notes/Spec/2107).
Doc 11 slices the work, doc 10 owns gates and suites, and milestones/ holds the working checklists (00-C0a through 12-C7).
When a checklist and the spec disagree, the spec wins and the checklist gets fixed.

## The loop for every slice

1. Pick the top unchecked box in the lowest open milestone file; tracking issues tamnd/chizu#1 through #13 map onto the milestones in the same order.
2. Branch from main, one slice per PR, never stack on an open PR, never commit to main.
3. If the slice bakes a constant, its lab (labs/<milestone>/NN_name) lands and runs first, with a sweep and a written verdict.
4. Open the PR with an imperative title and small focused commits.
5. After the merge, in the same sitting:
   - tick the box in ~/notes/Spec/2107/milestones/<file>.md and on the tracking issue,
   - comment on the tracking issue with what the PR settled, measured numbers included,
   - write the implementation note at ~/notes/Spec/2107/implementation/<pr-number>-<slug>.md; start it while the work happens, not after.
6. Run the full suite after every merge.

## Real numbers only

Perf and cost claims are measured on the real boxes, never on a laptop and never from paper arithmetic:

- server1: 4 cores, 6 GB RAM, 400 GB disk (AMD EPYC VPS)
- server2: 6 cores, 12 GB RAM, 200 GB disk (AMD EPYC VPS)
- server3: 8 cores, 24 GB RAM, 400 GB disk (AMD EPYC VPS); this is the gate box (E-box)

All three together are the standing fleet for fleet gates until bigger hardware exists.
Every verdict names the host it ran on.
Goals on this hardware: crawl 500-1000 pages/s per node (CR1 gates at 500), and the build pipeline as fast as the hardware allows with the stage breakdown published against the doc 06 decomposition.

## Code rules

- Flat packages, no internal/ anywhere; scripts/chizu-import-boundary.sh pins the edges and runs in CI.
- coldfmt, hotfmt, and wire import stdlib only; chain imports only s3c; nothing imports a plane except cmd/chizu; no AWS SDK.
- Human-readable Go that leans on the stdlib; gofmt -s clean; race detector on in CI.
- Every parser that touches bytes at rest or in flight gets fuzz coverage (format-fuzz, wire-fuzz).
- Tests that need a bucket read CHIZU_S3_ENDPOINT and skip when it is unset; CI's s3-suite lane provides MinIO.
