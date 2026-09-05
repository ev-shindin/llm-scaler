#!/usr/bin/env python3
"""Generate a GuideLLM `trace_synthetic` replay file from a phased shape/rate
spec (see test/benchmark/scenarios/shape_change_prefix_pressure.params.yaml
for the schema and field docs).

Each phase gets its own request rate and input/output token-length shape.
Both rate AND shape end up encoded purely as (timestamp, input_length,
output_length) rows -- GuideLLM's `--profile kind=replay` schedules from
these timestamps alone, so this file IS the schedule; there is no separate
--rate flag at run time.

Usage:
    python3 hack/benchmark/gen_shape_trace.py <params.yaml> [--out <path>]

--out overrides the params file's own `output.path` (which is otherwise
resolved relative to the params file's directory).
"""
import argparse
import json
import sys
from pathlib import Path

import numpy as np
import yaml


def sample_lengths(spec: dict, dist: str, n: int, rng: np.random.Generator) -> np.ndarray:
    if dist == "constant":
        return np.full(n, int(spec["mean"]))
    if dist == "uniform":
        lo, hi = int(spec["min"]), int(spec["max"])
        return rng.integers(lo, hi + 1, size=n)
    if dist == "normal":
        mean, std = float(spec["mean"]), float(spec["std_dev"])
        lo, hi = int(spec["min"]), int(spec["max"])
        vals = rng.normal(mean, std, size=n)
        return np.clip(np.round(vals), lo, hi).astype(int)
    raise ValueError(f"unknown length_distribution: {dist!r}")


def phase_arrival_offsets(duration_s: float, rate_rps: float, arrival: str, rng: np.random.Generator) -> np.ndarray:
    """Offsets (seconds, from this phase's own start) for every request that
    lands inside [0, duration_s). Count is itself random for poisson (a
    real Poisson process over a fixed window), fixed for constant."""
    if arrival == "constant":
        step = 1.0 / rate_rps
        n = int(duration_s // step)
        return np.arange(1, n + 1) * step
    if arrival == "poisson":
        offsets = []
        t = 0.0
        while True:
            t += rng.exponential(1.0 / rate_rps)
            if t >= duration_s:
                break
            offsets.append(t)
        return np.array(offsets)
    raise ValueError(f"unknown arrival_distribution: {arrival!r}")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("params_file")
    ap.add_argument("--out", default=None, help="Override output.path from the params file")
    args = ap.parse_args()

    params_path = Path(args.params_file).resolve()
    spec = yaml.safe_load(params_path.read_text())

    rng = np.random.default_rng(spec.get("seed"))
    arrival = spec.get("arrival_distribution", "poisson")
    length_dist = spec.get("length_distribution", "uniform")
    out_cfg = spec["output"]
    ts_col = out_cfg.get("timestamp_column", "timestamp")
    in_col = out_cfg.get("prompt_tokens_column", "input_length")
    out_col = out_cfg.get("output_tokens_column", "output_length")

    rows = []
    clock = 0.0  # running global offset -- each phase continues where the last left off
    for phase in spec["phases"]:
        name = phase["name"]
        duration_s = float(phase["duration_s"])
        rate_rps = float(phase["rate_rps"])

        offsets = phase_arrival_offsets(duration_s, rate_rps, arrival, rng)
        n = len(offsets)
        in_lens = sample_lengths(phase["input_tokens"], length_dist, n, rng)
        out_lens = sample_lengths(phase["output_tokens"], length_dist, n, rng)

        for off, il, ol in zip(offsets, in_lens, out_lens):
            rows.append({ts_col: round(clock + off, 6), in_col: int(il), out_col: int(ol)})

        actual_rate = n / duration_s if duration_s else 0.0
        print(
            f"[{name}] {n} requests over {duration_s:.0f}s "
            f"(target {rate_rps} req/s, actual {actual_rate:.3f} req/s), "
            f"input mean={in_lens.mean():.0f} output mean={out_lens.mean():.0f}",
            file=sys.stderr,
        )
        clock += duration_s

    out_path = Path(args.out) if args.out else (params_path.parent / out_cfg["path"])
    out_path.parent.mkdir(parents=True, exist_ok=True)
    with out_path.open("w") as f:
        for row in rows:
            f.write(json.dumps(row) + "\n")

    print(f"Wrote {len(rows)} rows -> {out_path}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
