#!/usr/bin/env python3
"""
harness_results.py -- turn one arm's inference-perf output into the
`requests.jsonl` + `meta.json` pair that two_model_report.py compares.

inference-perf writes, per run directory:

  per_request_lifecycle_metrics.json   one record per request: start_time and
                                       end_time as EPOCH seconds, info with
                                       output_token_times, error or null
  summary_lifecycle_metrics.json       load_summary (count, requested_rate,
                                       achieved_rate, schedule_delay) plus
                                       successes / failures
  stage_<n>_lifecycle_metrics.json     the same, per stage

Two run directories per arm, one per model, because the two models are driven
by two profiles with mirrored ladders.

This converter is deliberately thin and deliberately LOUD. It computes nothing
the harness already measured -- TTFT is the first output token's time minus the
request's start, and the driver's own scheduling delay is taken from
`load_summary.schedule_delay` rather than re-derived -- and it refuses rather
than emitting a short file when a field it needs is absent, because a report
built over a truncated conversion looks exactly like a report built over a run
where the cluster dropped the requests.
"""

import argparse
import json
import os
import sys

# Both layouts the harness has been seen to produce: the files at the root of
# the storage path, or one level down under `analysis/`. Searched rather than
# assumed, because guessing wrong yields "0 requests", which reads as a cluster
# that served nothing.
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
    base = os.path.dirname(shallow)
    republished = [os.path.join(base, "analysis", name)]
    extra = [h for h in hits[1:] if os.path.normcase(h) not in
             {os.path.normcase(r) for r in republished}]
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
    a model that returned 500.
    """
    if err is None:
        return ""
    if isinstance(err, str):
        return err
    if isinstance(err, dict):
        kind = err.get("type") or err.get("error_type") or err.get("name") or "error"
        msg = err.get("message") or err.get("msg") or err.get("detail") or ""
        return ("%s: %s" % (kind, msg)).strip().rstrip(":").strip()
    return str(err)


def rows_from(per_request, model_key, t0):
    """Report rows for one model.

    A record with no output token times did not deliver a first token, so it
    has no TTFT and counts as a failure -- including one the harness recorded
    without an `error`, which is what a torn stream looks like.
    """
    rows = []
    for rec in per_request:
        start = rec.get("start_time")
        if start is None:
            continue
        info = rec.get("info") or {}
        times = info.get("output_token_times") or []
        err = error_text(rec.get("error"))
        ttft = (times[0] - start) if times else None
        if ttft is not None and ttft < 0:
            # Cannot happen from one clock; if it does, the record is not
            # usable and must not be averaged into a percentile.
            ttft, err = None, err or "negative_ttft: first token before request start"
        if ttft is None and not err:
            err = "no_first_token: the response delivered no output tokens"
        rows.append({
            "model": model_key,
            "t_sched": start - t0,
            "ttft": ttft,
            "error": err,
            "end": (rec.get("end_time") - t0) if rec.get("end_time") is not None else None,
            "output_tokens": len(times),
        })
    rows.sort(key=lambda r: r["t_sched"])
    return rows


def pick_percentile(dist, want=95):
    """p95 out of a harness percentile block, or the nearest thing it reports.

    The block's key names have varied (`p95`, `95`, `percentile_95`), so this
    looks for the wanted percentile and then falls BACK TO A HIGHER one --
    never a lower one, because the value gates admissibility and reading p50
    where p95 was meant would pass a run the guard exists to stop.
    """
    if not isinstance(dist, dict):
        return None
    for key in ("p%d" % want, str(want), "percentile_%d" % want, "P%d" % want):
        if key in dist and isinstance(dist[key], (int, float)):
            return float(dist[key])
    for higher in (99, 999):
        for key in ("p%d" % higher, str(higher), "percentile_%d" % higher):
            if key in dist and isinstance(dist[key], (int, float)):
                return float(dist[key])
    if isinstance(dist.get("max"), (int, float)):
        return float(dist["max"])
    return None


def planned_arrivals(schedule, key):
    return sum(ph[key] * (ph["end"] - ph["start"]) for ph in schedule)


def convert(args):
    schedule = load_json(args.schedule)
    if not schedule:
        raise ValueError("the schedule is empty; there are no phases to report against")

    sides = {}
    for role, root in (("a", args.results_a), ("b", args.results_b)):
        pr_path = find_one(root, _PER_REQUEST)
        if pr_path is None:
            raise ValueError(
                "no %s under %s. inference-perf did not finish writing its report, so "
                "this arm has no per-request data -- see the container log before "
                "trusting anything else in the directory." % (_PER_REQUEST, root))
        sm_path = find_one(root, _SUMMARY)
        per_request = load_json(pr_path)
        if not isinstance(per_request, list) or not per_request:
            raise ValueError("%s holds no request records" % pr_path)
        sides[role] = {
            "per_request": per_request,
            "summary": load_json(sm_path) if sm_path else None,
            "per_request_path": pr_path,
            "summary_path": sm_path,
        }

    # One t0 for both models: the report buckets BOTH into the same phase table,
    # and two origins would put the two halves of an anti-phase run into
    # different phases.
    t0 = min(min(r["start_time"] for r in s["per_request"] if r.get("start_time") is not None)
             for s in sides.values())

    rows = []
    for role in ("a", "b"):
        rows.extend(rows_from(sides[role]["per_request"], role, t0))
    rows.sort(key=lambda r: r["t_sched"])

    issued = len(rows)
    planned = int(round(planned_arrivals(schedule, "rate_a")
                        + planned_arrivals(schedule, "rate_b")))

    queue_p95 = None
    achieved = {}
    for role in ("a", "b"):
        summary = sides[role]["summary"]
        if not summary:
            continue
        load_summary = summary.get("load_summary") or {}
        delay = pick_percentile(load_summary.get("schedule_delay"))
        if delay is not None:
            queue_p95 = delay if queue_p95 is None else max(queue_p95, delay)
        achieved[role] = {
            "count": load_summary.get("count"),
            "requested_rate": load_summary.get("requested_rate"),
            "achieved_rate": load_summary.get("achieved_rate"),
        }

    meta = {
        "t0": t0,
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
        # The driver's OWN queueing, as the driver measured it. The report
        # refuses the comparison above a threshold; reporting it here rather
        # than per row is what the harness makes available, and it is the same
        # quantity.
        "queue_delay_p95": queue_p95,
        "generator": "inference-perf",
        "achieved": achieved,
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
    p.add_argument("--schedule", required=True, help="the phase table, as JSON")
    p.add_argument("--out", required=True, help="requests.jsonl to write")
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

    served = sum(1 for r in rows if r["ttft"] is not None and not r["error"])
    print("%s: %d requests (%d served, %d failed) from %d planned; driver queueing p95 %s"
          % (args.arm, len(rows), served, len(rows) - served, meta["planned"],
             "-" if meta["queue_delay_p95"] is None
             else "%.0f ms" % (meta["queue_delay_p95"] * 1000)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
