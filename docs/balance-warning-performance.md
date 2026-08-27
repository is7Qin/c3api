# Balance-warning settlement performance evidence

**Date:** 2026-08-27
**Scope:** uncommitted balance-warning settlement change, measured through repository settlement commit.

## Environment and method

- Windows amd64, Go `go1.26.5`, AMD Ryzen 7 7840H, 8 cores / 16 logical processors, `GOMAXPROCS=16`.
- PostgreSQL 18.4 in `postgres:18-alpine`; no explicit container CPU or memory limit. `shared_buffers=128MB`, global `synchronous_commit=on`, with `SET LOCAL synchronous_commit TO off` in each settlement transaction.
- Database was 60 MB before measurement. `track_io_timing=off`; `pg_stat_statements` was unavailable because it was not preloaded.

The baseline is reconstructed from pre-feature HEAD `f83e1106e02506c142a9d3c65ca988b507ca4298`, not from an old binary or schema. Test-only code reverses only the warning SQL diff, uses the legacy seven-column scanner, and guards byte-for-byte equality with the HEAD SQL constants:

- Balance SHA-256: `e9ea120fef3b3a6e685763ecc74fad380885d7ada83ad90d6cfa8555e4375d2f`
- FEFO SHA-256: `9b754fe3ea890133e3b5a0015b40b69a200cfa3619b8847b7822ee4b71cdb0ba`

Both variants use the current schema, identical fixtures, real pgx transactions, one settlement CTE, result scanning, and commit. Fixture insertion and WAL probes are outside the benchmark timer. Each of the 10 authoritative samples ran in a fresh Go process and rebuilt the dedicated `public` schema. Five ran legacy first and five ran current first.

## Data shapes

- `single_500`: 500 usage rows for one user.
- `many_500`: 500 rows across 100 users, five rows each.
- `drain_8000_k4`: 8,000 rows across 40 users, 200 rows each, four concurrent bucket transactions, limit 8,000 per query.
- Balance has no temporary balance. FEFO gives each user 100 temporary balance, then spills to permanent balance.
- Disabled ends at permanent balance 1,000 with threshold 0. Non-crossing ends at 1,000 with threshold 500. Crossing ends at exactly 500 with threshold 500.

Current crossing cases assert one typed warning allocation per user. Disabled, non-crossing, and all legacy cases assert zero warnings.

## Timing and WAL

`p95` is nearest-rank across 10 independent samples. Deltas are current versus legacy medians. WAL is the median committed delta of `pg_current_wal_insert_lsn()` around the timed operation.

| Case | Legacy p50 / p95 ms | Current p50 / p95 ms | Median delta | Cold / warm delta | WAL p50 delta |
|---|---:|---:|---:|---:|---:|
| Balance single, disabled | 14.336 / 18.916 | 16.406 / 21.829 | +14.4% | +5.5% / +7.7% | -0.0% |
| Balance single, crossing | 13.397 / 21.893 | 14.480 / 15.610 | +8.1% | +9.7% / -3.2% | 0.0% |
| Balance many, crossing | 17.126 / 20.265 | 17.730 / 29.890 | +3.5% | -6.5% / +6.2% | 0.0% |
| Balance 8,000 / k=4, crossing | 1128.828 / 2154.013 | 1142.800 / 2166.618 | +1.2% | +0.1% / +9.0% | +0.4% |
| FEFO single, disabled | 20.957 / 28.114 | 19.886 / 24.918 | -5.1% | -1.3% / -1.6% | +0.5% |
| FEFO single, crossing | 16.324 / 28.071 | 17.779 / 24.863 | +8.9% | -4.0% / +4.8% | +0.6% |
| FEFO many, crossing | 33.014 / 57.061 | 32.845 / 55.562 | -0.5% | -14.2% / +5.1% | -0.6% |
| FEFO 8,000 / k=4, crossing | 127.723 / 2690.660 | 129.922 / 2851.864 | +1.7% | +1.3% / +3.7% | +0.3% |

Benchstat found no statistically significant `sec/op` difference in any case, `p >= 0.105`; the geomean was `+3.29%`. WAL median deltas ranged from `-0.6%` to `+0.6%`; benchstat WAL geomean was `+0.06%`, with no significant case. This supports that warnings add no new write path. It does not claim exactly zero physical-WAL overhead.

The 8,000-row cases are order-bimodal: the first large statement in a process can take 2 to 3 seconds and the second about 0.1 seconds. Swapping variant order swaps the slow result, so raw paired ratios are not a feature-overhead estimate. Cold and warm comparisons are the credible paired views. The current FEFO 8,000-row p95 was 2.852 s, below the 8 s batch-controller target and 10 s repository timeout. No relative regression budget is documented.

## Allocation, result, and database shape

| Shape | Legacy crossing | Current crossing | Current disabled/non-crossing |
|---|---:|---:|---:|
| Balance single | 2,864 B / 46 allocs | 3,360 B / 60 allocs | 3,216 B / 56 allocs |
| Balance many | 23,552 B / 548 allocs | 58,792 B / 1,163 allocs | 31,016 B / 755 allocs |
| FEFO single | 2,864 B / 46 allocs | 3,344 B / 60 allocs | 3,200 B / 55 allocs |
| FEFO many | 23,552 B / 548 allocs | 58,792 B / 1,163 allocs | 31,016 B / 755 allocs |
| FEFO 8,000 / k=4 | 45,328 B / 470 allocs | 69,976 B / 768 allocs | not measured |

Allocations rise because the result grows from seven to nine columns and crossing cases allocate typed warning events. The many-user crossing shape includes 100 events and slice growth. Post-commit channel submission, Redis claims, SMTP, and email sending are outside this repository benchmark.

Current results append two nullable columns, `threshold bigint` and `email text`. Disabled and non-crossing rows return NULLs; crossing user rows return both values. Row counts are 2 for single-user, 101 for many-user, and 44 for four buckets plus 40 users. Estimated pgx binary `DataRow` payloads are:

| Shape | Legacy | Current non-crossing | Current crossing |
|---|---:|---:|---:|
| Single | 182 B | 198 B | about 235 to 238 B |
| Many | 9,191 B | 9,999 B | about 13,789 to 14,089 B |
| 8,000 / k=4 | 4,004 B | not applicable | about 5,866 to 5,986 B |

These estimates exclude RowDescription, ReadyForQuery, packet, and transport framing. No packet capture was taken.

A single bucket uses four commands in both variants: `BEGIN`, `SET LOCAL`, the settlement CTE, and `COMMIT`. The four-bucket drain uses four concurrent settlement queries and 16 commands total, with no retry. Static comparison found identical DML clauses and write targets: Balance writes `users` and `usage_logs`; FEFO also writes `temp_balances` and writes `users` only for spill. Warnings add projections and a read-only `changed` CTE, with no extra round trip or write target.

## Reproduction and limits

With `TEST_DATABASE_URL` set to a PostgreSQL 18 test database:

```powershell
$env:GOMAXPROCS='16'
go test -run '^$' -bench 'BenchmarkPGBalanceWarningSettlement/balance/single_500/crossing' -benchtime=1x -count=1 -benchmem -p 1 ./internal/repository
go test -count=1 -p 1 ./internal/repository -run 'TestPGBalanceWarning|TestPGSettleBucketConcurrency' -v
for ($sample=1; $sample -le 10; $sample++) {
  if ($sample % 2 -eq 0) { $env:WARNING_BENCH_CURRENT_FIRST='1' } else { Remove-Item Env:WARNING_BENCH_CURRENT_FIRST -ErrorAction SilentlyContinue }
  go test -run '^$' -bench '^BenchmarkPGBalanceWarningSettlement$' -benchtime=1x -count=1 -benchmem -p 1 ./internal/repository
}
go run golang.org/x/perf/cmd/benchstat@latest -col /variant <raw-output>
```

The source validation recorded benchmark compilation, all 10 authoritative processes, and five related real-PostgreSQL tests passing in 21.666 s, with no `lsp_diagnostics` findings. Local Docker noise, 128 MB shared buffers, unavailable server-side I/O and statement statistics, no pprof, no packet capture, and database-global WAL LSN measurement limit precision. No intentional concurrent workload ran, but full-page images and background PostgreSQL activity can affect small WAL deltas.

The raw output, CSV summary, benchstat result, static DML comparison, and excluded noisy run remain local git-ignored evidence. This committed report is self-contained and does not depend on those artifacts. Source sections used: Scope and conclusion, Environment, Baseline construction, Data shapes, Commands, Timing and WAL results, Allocations, Round trips/writes/WAL/result shape, and Validation/artifacts/limitations.

**Conclusion:** the change has measurable allocation and result-byte growth, and a +3.29% timing geomean, but no statistically significant `sec/op` difference in the 10-sample study. It stays within the applicable absolute budgets; it is not evidence of exactly zero overhead.
