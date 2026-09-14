#!/usr/bin/env python3
"""
two_model_load.py -- anti-phase bursty load against TWO models at once, with
time-to-first-token measured from the moment each request was DUE.

WHY THIS IS NOT A llm-d-benchmark PROFILE
-----------------------------------------
The harness runs ONE model per treatment: a scenario names one `common.model`,
and its own reference says multi-stack scenarios are unsupported for a workload.
Two models whose bursts are OFFSET IN TIME cannot be expressed as two sequential
runs -- the whole point is what each model's ramp does to the other one's while
both are live. So the schedule lives here.

Everything else stays the suite's: the stacks come from `benchmark-standup`, the
pool from `deploy/warmpool.sh`, and the GPU timeseries from the driver.

THE MEASUREMENT, AND THE THREE WAYS IT GOES WRONG
-------------------------------------------------
Arrivals are a POISSON PROCESS at the phase's rate, issued whether or not
earlier requests have returned. A closed-loop driver (N workers, each sending
its next request when the last returns) cannot measure this run: when the server
slows, a closed-loop client sends LESS, the queue never grows, and TTFT stays
flat -- which makes a cold scale-up look free.

  1. The arrival times are PRE-COMPUTED, per model, from a per-model RNG stream,
     before a single request is sent. A driver that draws its next gap inside a
     wall-clock loop gives a different realisation on every run -- so the two
     arms of the comparison got different traffic despite an identical --seed,
     which is the one thing an A/B must not do.

  2. TTFT is measured from the SCHEDULED arrival, not from the moment a worker
     thread picked the request up. Timing from the thread start makes the
     driver's own backlog invisible, and a saturated arm then reports a SHORTER
     TTFT than a healthy one, because its late requests are timed from late.
     That is a bias in exactly the direction this run exists to measure. Both
     stamps are recorded; `queue_delay` is the driver's own contribution and the
     report refuses the comparison when it is material.

  3. A torn stream is an ERROR, not a fast request. A connection reset
     mid-response leaves a real TTFT and a short body, and counting it as a
     success lets the saturating arm shed its worst requests into a footnote
     while its p95 improves. Anything that did not deliver the tokens it asked
     for is marked, and the report prints the count beside every percentile.

TTFT comes from a STREAMING request, at the first chunk carrying non-empty
text -- not the first chunk: vLLM's first SSE frame is a role/empty delta, and
counting it reports a TTFT about one scheduler tick early, in both arms, which
shrinks the very gap being measured.

STANDARD LIBRARY ONLY. It runs inside a Pod in the namespace under test, because
a laptop's network is not part of the thing being measured -- and the image
guaranteed to be on those nodes is the model server's own, whose python has no
guaranteed httpx or aiohttp.
"""

import argparse
import json
import os
import random
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from http.client import HTTPConnection, HTTPSConnection
from urllib.parse import urlparse


# A phase is (start, end, rate for A, rate for B). Built rather than written out
# so the anti-phase property is structural: B's rate is A's from the OTHER
# level, always, and no edit can make the two models burst together while the
# file still claims they do not.
def build_schedule(phase_seconds, cycles, low_rps, high_rps, lead_in):
    """Anti-phase square wave: A low->high->low..., B high->low->high..."""
    phases = []
    t = 0
    if lead_in > 0:
        # Both models at the LOW rate first. Without it the run starts at the
        # instant of a burst and every arm measures a cold stack rather than a
        # scale-up: the models have never served a request, the pool has never
        # been asked for anything, and the first burst pays a first-request
        # penalty that has nothing to do with the pool.
        phases.append((t, t + lead_in, low_rps, low_rps))
        t += lead_in
    for _ in range(cycles):
        phases.append((t, t + phase_seconds, low_rps, high_rps))
        t += phase_seconds
        phases.append((t, t + phase_seconds, high_rps, low_rps))
        t += phase_seconds
    return phases


def rate_at(schedule, elapsed):
    """(rate_a, rate_b) at `elapsed` seconds, or (None, None) past the end."""
    for start, end, ra, rb in schedule:
        if start <= elapsed < end:
            return ra, rb
    return None, None


def phase_index_at(schedule, elapsed):
    for i, (start, end, _, _) in enumerate(schedule):
        if start <= elapsed < end:
            return i
    return -1


def plan_arrivals(schedule, model_key, seed):
    """Every arrival time for one model, as [(t_offset, phase_index)].

    Computed up front, from a stream of its OWN, so that:
      * the same --seed gives the same traffic in both arms, whatever the
        machine was doing at the time; and
      * the first arrival comes after an exponential gap rather than at t=0,
        where both models fired together and the run opened with a synchronised
        pair it never repeats.
    The gap is always drawn from the rate in force at the arrival that precedes
    it, so a phase boundary changes the rate for arrivals after it and never
    retroactively for one already placed.
    """
    rng = random.Random(seed)
    out = []
    t = 0.0
    total = schedule[-1][1]
    index = 0 if model_key == "a" else 1
    while True:
        rate = rate_at(schedule, t)[index]
        if rate is None:
            break
        if rate <= 0:
            # No arrivals in this phase: step to the next one rather than
            # spinning. (Not reachable with the shipped shapes, which refuse a
            # zero rate, but a schedule is data and this is data-driven.)
            nxt = [e for s, e, _, _ in schedule if s <= t < e]
            t = nxt[0] if nxt else total
            if t >= total:
                break
            continue
        t += rng.expovariate(rate)
        if t >= total:
            break
        out.append((t, phase_index_at(schedule, t)))
    return out


class Recorder:
    """Per-request rows, written once at the end.

    Held in memory rather than appended: an fsync per request puts disk latency
    inside the measurement, and these runs are tens of thousands of rows.
    """

    def __init__(self):
        self.rows = []
        self.lock = threading.Lock()
        self.inflight = 0
        self.max_inflight = 0

    def enter(self):
        with self.lock:
            self.inflight += 1
            if self.inflight > self.max_inflight:
                self.max_inflight = self.inflight

    def add(self, row):
        with self.lock:
            self.inflight -= 1
            self.rows.append(row)

    def count(self):
        with self.lock:
            return len(self.rows)

    def dump(self, path):
        with self.lock:
            rows = list(self.rows)
        rows.sort(key=lambda r: r["t_sched"])
        tmp = path + ".part"
        with open(tmp, "w") as fh:
            for r in rows:
                fh.write(json.dumps(r) + "\n")
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp, path)
        return len(rows)


def connect(url, timeout):
    parts = urlparse(url)
    host = parts.hostname
    port = parts.port or (443 if parts.scheme == "https" else 80)
    cls = HTTPSConnection if parts.scheme == "https" else HTTPConnection
    return cls(host, port, timeout=timeout), (parts.path or "/")


def one_request(rec, model_key, base_url, model_id, prompt, max_tokens,
                t0, t_sched_offset, phase, timeout):
    """Issue one streaming completion and record it.

    A new connection per request, deliberately: a pooled connection can be
    handed to a replica that is already saturated where a new one would have
    been routed elsewhere, and this run exists to measure what the router does
    during a ramp. Connection setup is inside TTFT, equally in both arms.
    """
    rec.enter()
    t_send = time.time()
    t_sched = t0 + t_sched_offset
    # The per-request DEADLINE. The socket timeout is per recv, so a stream
    # dripping one byte every timeout-1 seconds never expires and the drain at
    # the end of the run waits on it for as long as it cares to.
    deadline = t_send + timeout
    row = {
        "model": model_key,
        "phase": phase,
        # Relative to the run's origin, which is what the report and the GPU
        # samples are aligned on.
        "t_sched": round(t_sched_offset, 4),
        "queue_delay": round(t_send - t_sched, 4),
        "ttft": None,          # from the SCHEDULED arrival -- the honest one
        "ttft_send": None,     # from the moment it actually went out
        "total": None,
        "tokens": 0,
        "want_tokens": max_tokens,
        "done": False,
        "status": None,
        "error": None,
    }
    conn = None
    try:
        conn, path = connect(base_url, timeout)
        body = json.dumps({
            "model": model_id,
            "prompt": prompt,
            "max_tokens": max_tokens,
            "min_tokens": max_tokens,
            "temperature": 0.0,
            "stream": True,
            # Without this the engine stops at its own EOS and the decode work
            # per request varies with the model -- two models then run different
            # workloads while the report compares them as one.
            "ignore_eos": True,
        })
        conn.request("POST", path, body=body,
                     headers={"Content-Type": "application/json",
                              "Accept": "text/event-stream"})
        resp = conn.getresponse()
        row["status"] = resp.status
        if resp.status != 200:
            resp.read()
            row["error"] = "http %d" % resp.status
            return
        # The response is a BufferedReader over the socket, so this reads out of
        # an 8KB buffer rather than one syscall per byte.
        while True:
            if time.time() > deadline:
                row["error"] = "deadline"
                break
            line = resp.fp.readline()
            if not line:
                break
            line = line.strip()
            if not line.startswith(b"data:"):
                continue
            payload = line[5:].strip()
            if payload == b"[DONE]":
                row["done"] = True
                break
            try:
                obj = json.loads(payload)
            except ValueError:
                continue
            text = ""
            for choice in obj.get("choices", []):
                text += choice.get("text") or (choice.get("delta") or {}).get("content") or ""
            if not text:
                continue          # the role/empty first frame
            if row["ttft"] is None:
                now = time.time()
                row["ttft"] = round(now - t_sched, 4)
                row["ttft_send"] = round(now - t_send, 4)
            row["tokens"] += 1
    except Exception as exc:                       # noqa: BLE001 -- recorded, not raised
        row["error"] = "%s: %s" % (type(exc).__name__, exc)
    finally:
        row["total"] = round(time.time() - t_sched, 4)
        if row["error"] is None:
            if row["ttft"] is None:
                row["error"] = "no token"
            elif not row["done"] or row["tokens"] < max_tokens:
                # A TORN stream: a real TTFT and a short body. Counted as a
                # success it would let a saturating arm shed its worst requests
                # into a footnote while its percentiles improved.
                row["error"] = "truncated %d/%d" % (row["tokens"], max_tokens)
        if conn is not None:
            try:
                conn.close()
            except Exception:                      # noqa: BLE001
                pass
        rec.add(row)


def drive(args):
    schedule = build_schedule(args.phase_seconds, args.cycles,
                              args.low_rps, args.high_rps, args.lead_in)
    total = schedule[-1][1]
    rec = Recorder()

    # A SHARED PREFIX plus a per-arrival suffix. One reused prompt makes every
    # request after the first a full prefix-cache hit, which engineers away
    # prefill -- the exact cost a freshly scaled-up replica pays, and the thing
    # a bridge is supposed to spare. A fully random prompt is the other extreme,
    # a 100% miss that no real workload has. The suffix is derived from the
    # arrival index, so both arms send byte-identical traffic.
    prefix = ("benchmark " * max(1, args.input_tokens // 2)).strip()

    def prompt_for(key, i):
        return "%s [%s-%d]" % (prefix, key, i)

    plans = {k: plan_arrivals(schedule, k, args.seed + off)
             for k, off in (("a", 0), ("b", 1))}

    # Generous, because the alternative is worse: a bound that is reached makes
    # the driver closed-loop at exactly the moment the server slows, which is
    # the failure this file is built to avoid. Backlog is not prevented here --
    # it is MEASURED, as queue_delay, and the report refuses a comparison whose
    # driver was a material part of the number.
    workers = max(64, min(args.max_workers, int(4 * args.high_rps * 30)))
    pool = ThreadPoolExecutor(max_workers=workers)

    endpoints = {"a": (args.endpoint_a, args.model_a), "b": (args.endpoint_b, args.model_b)}
    t0 = time.time()

    print("schedule (%d phases, %ds total):" % (len(schedule), total), flush=True)
    for i, (s, e, ra, rb) in enumerate(schedule):
        print("  phase %d  %5ds..%5ds   A=%5.1f rps   B=%5.1f rps" % (i, s, e, ra, rb), flush=True)
    print("planned arrivals: A=%d  B=%d   workers=%d" %
          (len(plans["a"]), len(plans["b"]), workers), flush=True)
    print("T0=%.3f" % t0, flush=True)

    cursor = {"a": 0, "b": 0}
    last_report = -30
    while True:
        now = time.time()
        elapsed = now - t0
        if elapsed >= total:
            break
        fired = False
        for key in ("a", "b"):
            plan = plans[key]
            i = cursor[key]
            while i < len(plan) and plan[i][0] <= elapsed:
                at, phase = plan[i]
                url, model_id = endpoints[key]
                pool.submit(one_request, rec, key, url, model_id,
                            prompt_for(key, i), args.output_tokens,
                            t0, at, phase, args.timeout)
                i += 1
                fired = True
            cursor[key] = i
        if elapsed - last_report >= 30:
            last_report = elapsed - (elapsed % 30)
            ra, rb = rate_at(schedule, elapsed)
            print("  t=%5ds  phase=%d  A=%.1f B=%.1f rps  done=%d  inflight=%d" %
                  (int(elapsed), phase_index_at(schedule, elapsed), ra or 0, rb or 0,
                   rec.count(), rec.inflight), flush=True)
        if not fired:
            # Short enough that an arrival is never more than a few ms late at
            # these rates, long enough not to spin a core the workers need.
            time.sleep(0.002)

    issued = cursor["a"] + cursor["b"]
    planned = len(plans["a"]) + len(plans["b"])
    print("offering done at t=%ds: issued %d of %d planned arrivals; draining..."
          % (total, issued, planned), flush=True)
    pool.shutdown(wait=True)

    n = rec.dump(args.out)
    meta = {
        "t0": t0,
        "total_seconds": total,
        "schedule": [{"start": s, "end": e, "rate_a": ra, "rate_b": rb}
                     for s, e, ra, rb in schedule],
        "planned": planned,
        "issued": issued,
        "rows": n,
        "max_inflight": rec.max_inflight,
        "workers": workers,
        "arm": args.arm,
        "model_a": args.model_a,
        "model_b": args.model_b,
        "endpoint_a": args.endpoint_a,
        "endpoint_b": args.endpoint_b,
        "input_tokens": args.input_tokens,
        "output_tokens": args.output_tokens,
        "seed": args.seed,
    }
    with open(args.out + ".meta.json", "w") as fh:
        json.dump(meta, fh, indent=2)
        fh.flush()
        os.fsync(fh.fileno())

    # The line the driver waits for. It is printed AFTER both files are on disk
    # and fsynced, so seeing it means they can be copied.
    print("RESULTS-WRITTEN rows=%d max_inflight=%d" % (n, rec.max_inflight), flush=True)
    if issued < planned:
        print("WARNING: %d planned arrivals were never issued -- the driver, not the "
              "cluster, was the limit; this arm is NOT comparable."
              % (planned - issued), flush=True)
        return 3
    return 0


def main(argv):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--endpoint-a", required=True, help="full completions URL for model A")
    p.add_argument("--endpoint-b", required=True, help="full completions URL for model B")
    p.add_argument("--model-a", required=True)
    p.add_argument("--model-b", required=True)
    p.add_argument("--phase-seconds", type=int, default=360)
    p.add_argument("--cycles", type=int, default=2)
    p.add_argument("--lead-in", type=int, default=120)
    p.add_argument("--low-rps", type=float, default=2.0)
    p.add_argument("--high-rps", type=float, default=12.0)
    p.add_argument("--input-tokens", type=int, default=1000)
    p.add_argument("--output-tokens", type=int, default=200)
    p.add_argument("--timeout", type=int, default=300)
    p.add_argument("--max-workers", type=int, default=1024)
    p.add_argument("--seed", type=int, default=1729)
    p.add_argument("--arm", default="unknown", help="pool|nopool, recorded in the meta")
    p.add_argument("--out", default="/results/requests.jsonl")
    args = p.parse_args(argv)
    if args.low_rps >= args.high_rps:
        # The scenario is a burst. Equal rates make it a flat run that the
        # report would still describe as bursty, phase by phase.
        print("ERROR: --low-rps (%s) must be below --high-rps (%s); with both equal "
              "there is no burst to bridge." % (args.low_rps, args.high_rps),
              file=sys.stderr)
        return 2
    if args.cycles < 1:
        print("ERROR: --cycles must be at least 1", file=sys.stderr)
        return 2
    if args.endpoint_a == args.endpoint_b:
        # llm-d routes a multi-model stack by PATH PREFIX (/{stack}/v1/...), not
        # by the model name in the body. One URL for both models sends every
        # request to whichever stack owns that path, and the other model's rows
        # come back 404 -- a full run of failures that reads as a pool result.
        print("ERROR: --endpoint-a and --endpoint-b are the same URL (%s). A "
              "multi-model llm-d stack routes by path prefix, so each model has "
              "its own." % args.endpoint_a, file=sys.stderr)
        return 2
    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    return drive(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
