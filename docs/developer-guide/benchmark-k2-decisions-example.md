# Example: K1/K2 Capacity Decision Report

Sample output of [`hack/benchmark/dump_k2_decisions.py`](../../hack/benchmark/dump_k2_decisions.py),
run against a `prefill_heavy` benchmark (`Qwen/Qwen3-0.6B`, single variant, TP=1) on a real
OpenShift cluster, with `GLOBAL_OPT_INTERVAL=15s` and the EPP `flowControl` gate enabled. Kept
here as a worked example of what the report looks like; run the script yourself against a fresh
results directory to get current numbers.

The report is one table: one row per optimize cycle, totalled across every ready replica of the
variant that cycle (`N`), not one row per replica. `Decision` is the actual, post-enforcement
target replica count for that cycle (`D9` = decided 9) — read alongside `N` it shows the
controller reacting to demand before the replica count catches up (e.g. `13:04:30`: still 1
ready replica, but already decided 7).

Two things decide whether the report comes back populated:

- **Controller verbosity.** The two per-replica lines are logged at `V(logging.DEFAULT)`, the
  verbosity the shipped deployment runs at. A controller started with `-v=1` or lower
  suppresses them and the report is empty.
- **`--cycle-gap`** (default 3s) is the silence that separates two optimize cycles. It must
  stay below `GLOBAL_OPT_INTERVAL`, which ships at 15s but may be configured as low as 1s —
  so the script detects a gap that spanned more than one cycle and narrows it automatically,
  reporting what it chose on stderr. It will not narrow below 1s, since log timestamps are
  second-granularity and a cycle straddling a second boundary would then split in two.

---

# K1/K2 Capacity Decision Report

Window: 2026-08-15T13:03:01+00:00 -> 2026-08-15T13:16:56+00:00
Total events captured: 796

## Variant: qwen-qwe-3db867ce-en3-0-6b-decode-wva

One row per optimize cycle, totalled across every ready replica of this variant that cycle (N). KVinUse/LocalQ/EPPq/TotalDemand are all in tokens; Priority lists every k2 tier that fired across N replicas this cycle (P1-obs=observed, P2-hist=historical average, P3-k2=derived from deployment args, P4-k1=no signal, memory-bound only). Time is HH:MM:SS on the run date above.

Legend — Bound: k1=memory-bound won, k2=compute-bound won.  Decision: DN = the controller decided N replicas (post scale-to-zero/min-replica enforcement).

| Time     | N | Priority       | k2     | k1     | Bound | KVinUse | LocalQ  | EPPq      | TotalDemand | Decision |
|----------|---|----------------|--------|--------|-------|---------|---------|-----------|-------------|----------|
| 13:03:15 | 1 | P4-k1          | 487065 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:03:30 | 1 | P4-k1          | 487065 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:03:45 | 1 | P4-k1          | 487065 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:04:00 | 1 | P4-k1          | 487065 | 487065 | k1    | 601663  | 0       | 613507    | 1215170     | D3       |
| 13:04:15 | 1 | P4-k1          | 487065 | 487065 | k1    | 601663  | 0       | 246561    | 848224      | D3       |
| 13:04:30 | 1 | P1-obs         | 605760 | 487065 | k1    | 605760  | 1277040 | 794884.25 | 2677684.25  | D7       |
| 13:04:45 | 1 | P1-obs         | 605760 | 487065 | k1    | 605760  | 1277040 | 1421034   | 3303834     | D8       |
| 13:05:00 | 1 | P1-obs         | 605760 | 487065 | k1    | 605760  | 1277040 | 1731492.5 | 3614292.5   | D9       |
| 13:05:15 | 2 | P1-obs,P4-k1   | 605760 | 487065 | k1    | 605760  | 1277040 | 60607.75  | 1943407.75  | D9       |
| 13:05:30 | 3 | P1-obs,P2-hist | 606784 | 487065 | k1    | 1215296 | 600960  | 127816    | 1944072     | D9       |
| 13:05:45 | 5 | P1-obs,P2-hist | 606784 | 487065 | k1    | 1216512 | 600960  | 155140.25 | 1972612.25  | D9       |
| 13:06:00 | 7 | P1-obs,P2-hist | 607594 | 487065 | k1    | 1216512 | 565904  | 47243.25  | 1829659.25  | D8       |
| 13:06:16 | 8 | P1-obs,P2-hist | 607594 | 487065 | k1    | 2095453 | 1161856 | 0         | 3257309     | D9       |
| 13:06:31 | 9 | P1-obs,P2-hist | 607232 | 487065 | k1    | 3120264 | 595952  | 0         | 3716216     | D9       |
| 13:06:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 3462637 | 0       | 0         | 3462637     | D9       |
| 13:07:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 3087941 | 0       | 0         | 3087941     | D9       |
| 13:07:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 2783780 | 0       | 0         | 2783780     | D9       |
| 13:07:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 2476036 | 0       | 0         | 2476036     | D8       |
| 13:07:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 2457986 | 0       | 0         | 2457986     | D8       |
| 13:08:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 2092507 | 0       | 0         | 2092507     | D7       |
| 13:08:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 1318602 | 0       | 0         | 1318602     | D4       |
| 13:08:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 1240834 | 0       | 0         | 1240834     | D4       |
| 13:08:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 1260998 | 0       | 0         | 1260998     | D4       |
| 13:09:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 1171772 | 0       | 0         | 1171772     | D4       |
| 13:09:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 1141367 | 0       | 0         | 1141367     | D4       |
| 13:09:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 1153209 | 0       | 0         | 1153209     | D4       |
| 13:09:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 1176571 | 0       | 0         | 1176571     | D4       |
| 13:10:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 1178555 | 0       | 0         | 1178555     | D4       |
| 13:10:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 1071024 | 0       | 0         | 1071024     | D4       |
| 13:10:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 994985  | 0       | 0         | 994985      | D3       |
| 13:10:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 866523  | 0       | 0         | 866523      | D3       |
| 13:11:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 897309  | 0       | 0         | 897309      | D3       |
| 13:11:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 891293  | 0       | 0         | 891293      | D3       |
| 13:11:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 922657  | 0       | 0         | 922657      | D3       |
| 13:11:46 | 9 | P2-hist        | 607308 | 487065 | k1    | 1080945 | 0       | 0         | 1080945     | D4       |
| 13:12:01 | 9 | P2-hist        | 607308 | 487065 | k1    | 1021676 | 0       | 0         | 1021676     | D3       |
| 13:12:16 | 9 | P2-hist        | 607308 | 487065 | k1    | 1084146 | 0       | 0         | 1084146     | D4       |
| 13:12:31 | 9 | P2-hist        | 607308 | 487065 | k1    | 1065712 | 0       | 0         | 1065712     | D3       |
| 13:12:46 | 8 | P2-hist        | 607308 | 487065 | k1    | 909983  | 0       | 0         | 909983      | D2       |
| 13:13:01 | 8 | P2-hist        | 607308 | 487065 | k1    | 1115701 | 0       | 0         | 1115701     | D2       |
| 13:13:16 | 7 | P2-hist        | 607308 | 487065 | k1    | 973798  | 0       | 0         | 973798      | D1       |
| 13:13:31 | 4 | P2-hist        | 607308 | 487065 | k1    | 1189757 | 0       | 0         | 1189757     | D2       |
| 13:13:46 | 4 | P2-hist        | 607308 | 487065 | k1    | 1359951 | 0       | 0         | 1359951     | D3       |
| 13:14:01 | 4 | P2-hist        | 607308 | 487065 | k1    | 1359951 | 0       | 0         | 1359951     | D3       |
| 13:14:16 | 4 | P2-hist        | 607308 | 487065 | k1    | 1359951 | 0       | 0         | 1359951     | D4       |
| 13:14:31 | 4 | P2-hist        | 607308 | 487065 | k1    | 367975  | 0       | 0         | 367975      | D2       |
| 13:14:46 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:15:01 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:15:16 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:15:31 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:15:46 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:16:01 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:16:16 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:16:31 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
| 13:16:46 | 4 | P2-hist        | 607308 | 487065 | k1    | 0       | 0       | 0         | 0           | D1       |
