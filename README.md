# chizu (地図)

A web search engine in Go, built from scratch: crawler, index builder, and serving fleet.

The contract is a 10ms answer for every query, served from an index that lives on NVMe as one immutable `.hot` file per shard generation, with the durable truth in immutable columnar `.cold` segments on object storage.
Object storage is never on the query path.
Coordination is a CAS commit chain on the bucket itself; there is no database and no coordination service anywhere in the system.

The full spec is Spec 2107 in the project notes: formats, crawl, indexing, serving, ranking, the cost model, and a gate registry where every quantitative claim must have a measured row.
Work lands milestone by milestone; each milestone has a tracking issue (#1 through #13) that mirrors its checklist and links every merged PR.
Perf and cost numbers quoted anywhere in this repo are measured on the project's real boxes, never extrapolated.

## Layout

Flat packages, no `internal/`, import edges pinned by `scripts/chizu-import-boundary.sh` in CI:

- `coldfmt`, `hotfmt`, `wire`: the three formats, stdlib only
- `s3c`, `chain`: the object-storage client and the CAS coordination chain
- crawl, build, and serve planes arrive with milestones C5, C3, and C4
- `cmd/chizu`: the single binary for every plane
- `labs/`: microbenchmarks, one directory per milestone, each with a sweep and a written verdict

## Status

Pre-C0a: the spec is done, the repo is bootstrapped, nothing serves queries yet.

## License

MIT
