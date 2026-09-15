#!/usr/bin/env python3
"""
harness_results.py -- turn one arm's inference-perf output into the
`requests.jsonl` + `meta.json` pair that two_model_report.py compares.

WHERE EACH NUMBER COMES FROM, and why it is not the obvious place.

inference-perf v0.6.1 writes, per run directory:

  per_request_lifecycle_metrics.json   one record per request: start_time and
                                       end_time, `info` with input/output token
                                       counts and the raw response chunks, and
                                       `error` with an `error_type`
  stage_<n>_lifecycle_metrics.json     per stage: load_summary (count,
                                       requested/achieved rate, schedule_delay)
                                       plus `successes.latency`, which is where
                                       `time_to_first_token` lives
  summary_lifecycle_metrics.json       the same, over the whole run

**TTFT is per STAGE, not per request.** The per-request records carry no token
timestamps at all in this version -- measured on CoreWeave: 210 records, every
one with `info.response_metrics.response_chunks` and no time on any chunk. So
latency cannot be re-cut into windows after the fact, and the profile makes the
windows that matter into stages instead. This converter reads the distributions
the harness computed; it does not recompute them from data that is not there.

**start_time is a MONOTONIC clock, not epoch** -- measured: 10655087.75, an
uptime. It orders requests and nothing else. The run's origin on the wall clock
comes from `--t0`, the start barrier the driver handed both containers, and
that is what the GPU samples are aligned against.

The converter refuses rather than emitting a short file when something it needs
is absent: a report built over a truncated conversion looks exactly like a
report built over a run where the cluster dropped the requests.
"""

import argparse
import json
import os
import sys

_PER_REQUEST = "per_request_lifecycle_metrics.json"
_SUMMARY = "summary_lifecycle_metrics.json"


def find_one(root, name):
    """The single file called `name` under `root`, or None.

    inference-perf writes its report either at the root of the storage path or
    one level down under `analysis/`, and the harness's own readers try both --
    so BOTH may be present for one run, and a copy that differs from the
    shallowest only by that segment is the same run republished, not a second
    one.

    Anything else is refused. inference-perf writes one run per storage path
    here, so a second copy elsewhere means the directory was reused between
    arms, and picking one would compare an arm against part of itself.
    """
    hits = []
    for dirpath, _dirnames, filenames in os.walk(root):
        if name in filenames:
            hits.append(os.path.join(dirpath, name))
    if not hits:
        return None
    hits.sort(key=lambda p: (p.count(os.sep), p))
    shallow = hits[0]
    republished = os.path.join(os.path.dirname(shallow), "analysis", name)
    extra = [h for h in hits[1:]
             if os.path.normcase(h) != os.path.normcase(republished)]
    if extra:
        raise ValueError(
            "%d copies of %s under %s -- the directory holds more than one run, "
            "and picking one would compare an arm against part of itself:\n  %s"
            % (len(hits), name, root, "\n  ".join(hits)))
    return shallow


def load_json(path):
    with open(path) as fh:
        return json.load(fh)


def error_text(err):
    """A single string for an inference-perf error object.

    Kept in `<type>: <message>` shape because the report classifies CLIENT-side
    failures by the leading token -- a run thinned by the driver's own sockets
    is not a measurement of the cluster, and it has to be distinguishable from
    a model that returned 500. The type name is inference-perf's own
    `error_type` (e.g. `ClientConnectorDNSError`), not a guess.
    """
    if err is None:
        return ""
    if isinstance(err, str):
        return err
    if isinstance(err, dict):
        kind = err.get("error_type") or err.get("type") or err.get("name") or "error"
        msg = err.get("error_msg") or err.get("message") or err.get("msg") or ""
        return ("%s: %s" % (kind, msg)).strip().rstrip(":").strip()
    return str(err)


def rows_from(per_request, model_key, origin):
    """One row per request: what failed, and when relative to the run's start.

    NO TTFT: this version of inference-perf does not record one per request.
    The rows exist for counts and for telling the driver's own socket failures
    apart from the cluster's, which is what `error_type` gives.
    """
    rows = []
    for rec in per_request:
        start = rec.get("start_time")
        if start is None:
            continue
        info = rec.get("info") or {}
        rows.append({
            "model": model_key,
            "t_rel": start - origin,
            "error": error_text(rec.get("error")),
            "input_tokens": info.get("input_tokens"),
            "output_tokens": (info.get("response_metrics") or {}).get("output_tokens"),
        })
    rows.sort(key=lambda r: r["t_rel"])
    return rows


def pick_percentile(dist, want=95):
    """One percentile out of a harness percentile block.

    inference-perf writes `median`, `p95`, `p99`, `min`, `max` and friends;
    other shapes have used bare numbers or `percentile_95`. Looks for the wanted
    percentile, then falls BACK TO A HIGHER one -- never a lower one, because
    these values gate admissibility and reading p50 where p95 was meant would
    pass a run the guard exists to stop.
    """
    if not isinstance(dist, dict):
        return None
    preferred = {50: ("median", "p50", "50", "percentile_50")}.get(
        want, ("p%d" % want, str(want), "percentile_%d" % want))
    for key in preferred:
        if key in dist and isinstance(dist[key], (int, float)):
            return float(dist[key])
    for higher in ("p99", "99", "percentile_99", "p99.9", "99.9"):
        if higher in dist and isinstance(dist[higher], (int, float)):
            return float(dist[higher])
    if isinstance(dist.get("max"), (int, float)):
        return float(dist["max"])
    return None


def window_from(doc):
    """{n, failed, p50, p95, p99, max, ...} out of one stage or summary file.

    `None` for every latency when the window served nothing: a stage where
    every request failed has no TTFT, and reporting one as 0 would make the
    worst window look like the best.
    """
    load_summary = doc.get("load_summary") or {}
    successes = doc.get("successes") or {}
    failures = doc.get("failures") or {}
    ttft = ((successes.get("latency") or {}).get("time_to_first_token")) or {}
    return {
        "n": successes.get("count", 0),
        "failed": failures.get("count", 0),
        "p50": pick_percentile(ttft, 50),
        "p95": pick_percentile(ttft, 95),
        "p99": pick_percentile(ttft, 99),
        "max": ttft.get("max") if isinstance(ttft.get("max"), (int, float)) else None,
        "requested_rate": load_summary.get("requested_rate"),
        "achieved_rate": load_summary.get("achieved_rate"),
        "schedule_delay_p95": pick_percentile(load_summary.get("schedule_delay")),
    }


def stage_windows(root, n_expected):
    """One window per stage, in order, refusing a count that does not match.

    A missing stage file is not a gap to skip: stage N of the schedule and
    stage N of the report have to be the same window, and a run that wrote
    fewer stages than the profile asked for did not run the schedule the report
    is about to describe.
    """
    windows = []
    for i in range(n_expected):
        path = find_one(root, "stage_%d_lifecycle_metrics.json" % i)
        if path is None:
            raise ValueError(
                "stage %d of %d has no report under %s. The run did not complete the "
                "schedule, so the windows the report would describe are not the "
                "windows that ran." % (i, n_expected, root))
        windows.append(window_from(load_json(path)))
    return windows


def planned_arrivals(schedule, key):
    return sum(ph[key] * (ph["end"] - ph["start"]) for ph in schedule)


def convert(args):
    schedule = load_json(args.schedule)
    if not schedule:
        raise ValueError("the schedule is empty; there are no stages to report against")

    sides = {}
    for role, root in (("a", args.results_a), ("b", args.results_b)):
        pr_path = find_one(root, _PER_REQUEST)
        if pr_path is None:
            raise ValueError(
                "no %s under %s. inference-perf did not finish writing its report, so "
                "this arm has no per-request data -- see the container log before "
                "trusting anything else in the directory." % (_PER_REQUEST, root))
        sm_path = find_one(root, _SUMMARY)
        if sm_path is None:
            raise ValueError(
                "no %s under %s. The whole-run latency distribution lives there and "
                "cannot be recomputed: this version records no per-request token "
                "times." % (_SUMMARY, root))
        per_request = load_json(pr_path)
        if not isinstance(per_request, list) or not per_request:
            raise ValueError("%s holds no request records" % pr_path)
        sides[role] = {
            "per_request": per_request,
            "overall": window_from(load_json(sm_path)),
            "stages": stage_windows(root, len(schedule)),
            "per_request_path": pr_path,
            "summary_path": sm_path,
        }

    origin = min(min(r["start_time"] for r in s["per_request"]
                     if r.get("start_time") is not None)
                 for s in sides.values())

    rows = []
    for role in ("a", "b"):
        rows.extend(rows_from(sides[role]["per_request"], role, origin))
    rows.sort(key=lambda r: r["t_rel"])

    issued = len(rows)
    planned = int(round(planned_arrivals(schedule, "rate_a")
                        + planned_arrivals(schedule, "rate_b")))

    # The WORST of everything the generator reported about its own lateness --
    # whole run and every stage. A driver that kept up on average while falling
    # behind through one burst was late exactly where it mattered.
    delays = []
    for role in ("a", "b"):
        for w in [sides[role]["overall"]] + sides[role]["stages"]:
            if isinstance(w["schedule_delay_p95"], (int, float)):
                delays.append(w["schedule_delay_p95"])
    queue_p95 = max(delays) if delays else None

    meta = {
        # The WALL-CLOCK origin, from the driver's start barrier. The harness's
        # own timestamps are monotonic and cannot be aligned to GPU samples.
        "t0": args.t0,
        "total_seconds": schedule[-1]["end"],
        "schedule": schedule,
        "planned": planned,
        "issued": issued,
        "rows": len(rows),
        "arm": args.arm,
        "model_a": args.model_a,
        "model_b": args.model_b,
        "input_tokens": args.input_tokens,
        "output_tokens": args.output_tokens,
        "seed": args.seed,
        "prefix_groups": args.prefix_groups,
        "queue_delay_p95": queue_p95,
        "generator": "inference-perf",
        # What the report prints: the harness's own distributions, per stage
        # and over the whole run.
        "windows": {role: {"overall": sides[role]["overall"],
                           "stages": sides[role]["stages"]}
                    for role in ("a", "b")},
        "sources": {role: {"per_request": sides[role]["per_request_path"],
                           "summary": sides[role]["summary_path"]}
                    for role in ("a", "b")},
    }
    return rows, meta


def main(argv):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--results-a", required=True, help="model A's inference-perf output dir")
    p.add_argument("--results-b", required=True, help="model B's inference-perf output dir")
    p.add_argument("--schedule", required=True, help="the stage table, as JSON")
    p.add_argument("--out", required=True, help="requests.jsonl to write")
    p.add_argument("--t0", type=float, required=True,
                   help="epoch both containers started; the harness's own clock is monotonic")
    p.add_argument("--arm", default="unknown")
    p.add_argument("--model-a", default="A")
    p.add_argument("--model-b", default="B")
    p.add_argument("--input-tokens", type=int, default=0)
    p.add_argument("--output-tokens", type=int, default=0)
    p.add_argument("--seed", type=int, default=0)
    p.add_argument("--prefix-groups", type=int, default=0)
    args = p.parse_args(argv)

    try:
        rows, meta = convert(args)
    except (ValueError, OSError) as exc:
        print("harness_results: %s" % exc, file=sys.stderr)
        return 2

    with open(args.out, "w") as fh:
        for row in rows:
            fh.write(json.dumps(row) + "\n")
    with open(args.out + ".meta.json", "w") as fh:
        json.dump(meta, fh, indent=2)

    served = sum(w["overall"]["n"] for w in meta["windows"].values())
    failed = sum(w["overall"]["failed"] for w in meta["windows"].values())
    print("%s: %d requests (%d served, %d failed) from %d planned; driver queueing p95 %s"
          % (args.arm, len(rows), served, failed, meta["planned"],
             "-" if meta["queue_delay_p95"] is None
             else "%.0f ms" % (meta["queue_delay_p95"] * 1000)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
