#!/usr/bin/env python3
"""
two_model_plots.py -- the two-model report's numbers, drawn.

Reads the JSON that `two_model_report.py --json` writes and emits three SVGs
next to it. Standard library only, on purpose: this runs wherever the report
runs, which is the operator's laptop after a cluster run, and a plot that needs
a package install is a plot that does not get made.

  ttft-rises.svg     p95 (bar) and p50 (tick) time-to-first-token in each rise
                     window, one group per scale-up event, one bar per arm
  gpu-seconds.svg    accelerator-seconds per arm: what the models used, what
                     the pool held, and how much of that was actually lent
  fleet-timeline.svg accelerators held over the run, per arm, with the rises
                     marked -- where the cost in gpu-seconds comes from

Every number is the report's own; nothing is recomputed here.
"""

import argparse
import json
import os
import sys

COLORS = {"nopool": "#7f7f7f", "pool": "#1f77b4", "floor": "#ff7f0e"}
FONT = "font-family='Helvetica, Arial, sans-serif'"


def esc(s):
    return (str(s).replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;"))


class SVG:
    def __init__(self, w, h):
        self.w, self.h = w, h
        self.parts = ["<svg xmlns='http://www.w3.org/2000/svg' width='%d' height='%d' "
                      "viewBox='0 0 %d %d' %s font-size='12'>" % (w, h, w, h, FONT),
                      "<rect width='%d' height='%d' fill='white'/>" % (w, h)]

    def rect(self, x, y, w, h, fill, opacity=1.0, stroke="none", title=None):
        t = "<title>%s</title>" % esc(title) if title else ""
        self.parts.append("<rect x='%.1f' y='%.1f' width='%.1f' height='%.1f' fill='%s' "
                          "fill-opacity='%.2f' stroke='%s'>%s</rect>"
                          % (x, y, max(w, 0), max(h, 0), fill, opacity, stroke, t))

    def line(self, x1, y1, x2, y2, stroke="#333", width=1.0, dash=None):
        d = " stroke-dasharray='%s'" % dash if dash else ""
        self.parts.append("<line x1='%.1f' y1='%.1f' x2='%.1f' y2='%.1f' stroke='%s' "
                          "stroke-width='%.1f'%s/>" % (x1, y1, x2, y2, stroke, width, d))

    def path(self, points, stroke, width=1.5):
        if not points:
            return
        d = "M %.1f %.1f " % points[0] + " ".join("L %.1f %.1f" % p for p in points[1:])
        self.parts.append("<path d='%s' fill='none' stroke='%s' stroke-width='%.1f'/>"
                          % (d, stroke, width))

    def text(self, x, y, s, size=12, anchor="start", fill="#222", rotate=None, weight=None):
        r = " transform='rotate(%d %.1f %.1f)'" % (rotate, x, y) if rotate else ""
        w = " font-weight='%s'" % weight if weight else ""
        self.parts.append("<text x='%.1f' y='%.1f' font-size='%d' text-anchor='%s' fill='%s'%s%s>%s</text>"
                          % (x, y, size, anchor, fill, r, w, esc(s)))

    def write(self, path):
        self.parts.append("</svg>")
        with open(path, "w") as fh:
            fh.write("\n".join(self.parts) + "\n")


def nice_max(v):
    """A round axis maximum a little above v."""
    if v <= 0:
        return 1.0
    mag = 10 ** int(len(str(int(v))) - 1)
    for m in (1, 2, 2.5, 5, 10):
        if m * mag >= v * 1.05:
            return m * mag
    return 10 * mag


def rise_label(models, key_at):
    key, at = key_at.split("@")
    name = models[key].split("/")[-1]
    return "%s @%ss" % (name, at)


def plot_ttft(data, out):
    arms = list(data["arms"].keys())
    models = data["models"]
    keys = []
    for k in ("a", "b"):
        for i in data["rises"][k]:
            keys.append("%s@%d" % (k, data["schedule"][i]["start"]))
    keys.sort(key=lambda s: int(s.split("@")[1]))
    if not keys:
        return
    vmax = 0.0
    for arm in arms:
        for k in keys:
            w = data["arms"][arm]["rises"].get(k)
            if w and w.get("p95") is not None:
                vmax = max(vmax, w["p95"] * 1000)
    ymax = nice_max(vmax)
    left, right, top, bottom = 70, 20, 50, 70
    gw = 150
    W = left + right + gw * len(keys)
    H = 360
    svg = SVG(W, H)
    ph = H - top - bottom

    def y_of(ms):
        return top + ph - (ms / ymax) * ph

    svg.text(left, 22, "Time to first token in each rise window (first 90s after the rate goes up)", 14, weight="bold")
    svg.text(left, 38, "bar = p95, tick = p50; one group per scale-up event, one run each", 11, fill="#555")
    for i in range(6):
        v = ymax * i / 5
        y = y_of(v)
        svg.line(left, y, W - right, y, "#ddd")
        svg.text(left - 6, y + 4, "%d" % v, 11, anchor="end")
    svg.text(16, top + ph / 2, "ms", 11, anchor="middle", rotate=-90)
    n = len(arms)
    bw = (gw - 40) / n
    for gi, k in enumerate(keys):
        gx = left + gi * gw + 20
        svg.text(gx + (gw - 40) / 2, H - bottom + 18, rise_label(models, k), 11, anchor="middle")
        for ai, arm in enumerate(arms):
            w = data["arms"][arm]["rises"].get(k)
            x = gx + ai * bw
            if not w or w.get("p95") is None:
                svg.text(x + bw / 2, y_of(0) - 4, "-", 11, anchor="middle")
                continue
            p95 = w["p95"] * 1000
            p50 = (w.get("p50") or 0) * 1000
            svg.rect(x + 2, y_of(p95), bw - 4, y_of(0) - y_of(p95), COLORS.get(arm, "#999"),
                     title="%s %s: p95 %.0f ms, p50 %.0f ms, %d served, %d failed"
                           % (arm, rise_label(models, k), p95, p50, w.get("n", 0), w.get("failed", 0)))
            svg.line(x + 2, y_of(p50), x + bw - 2, y_of(p50), "#111", 2.0)
            svg.text(x + bw / 2, y_of(p95) - 3, "%.0f" % p95, 10, anchor="middle")
            if w.get("failed"):
                svg.text(x + bw / 2, H - bottom + 32, "%d failed" % w["failed"], 10, anchor="middle", fill="#c00")
    lx = left
    for arm in arms:
        svg.rect(lx, H - 22, 12, 12, COLORS.get(arm, "#999"))
        svg.text(lx + 16, H - 12, arm, 11)
        lx += 80
    svg.write(os.path.join(out, "ttft-rises.svg"))


def plot_gpu_seconds(data, out):
    arms = [a for a in data["arms"] if data["arms"][a].get("gpu_seconds")]
    if not arms:
        return
    vmax = max(data["arms"][a]["gpu_seconds"]["all"] for a in arms)
    ymax = nice_max(vmax)
    left, right, top, bottom = 80, 20, 50, 50
    gw = 140
    W = left + right + gw * len(arms) + 160
    H = 340
    svg = SVG(W, H)
    ph = H - top - bottom

    def y_of(v):
        return top + ph - (v / ymax) * ph

    svg.text(left, 22, "Accelerator-seconds over the run, per arm", 14, weight="bold")
    svg.text(left, 38, "insurance is a cost: the pool's Pods count whether or not they were lent", 11, fill="#555")
    for i in range(6):
        v = ymax * i / 5
        y = y_of(v)
        svg.line(left, y, left + gw * len(arms), y, "#ddd")
        svg.text(left - 6, y + 4, "%d" % v, 11, anchor="end")
    svg.text(18, top + ph / 2, "GPU-seconds", 11, anchor="middle", rotate=-90)
    for ai, arm in enumerate(arms):
        g = data["arms"][arm]["gpu_seconds"]
        x = left + ai * gw + 30
        bw = gw - 60
        models_part = g["all"] - g["pool"]
        col = COLORS.get(arm, "#999")
        svg.rect(x, y_of(models_part), bw, y_of(0) - y_of(models_part), col,
                 title="%s: model replicas %.0f GPU-s" % (arm, models_part))
        if g["pool"] > 0:
            held = g["pool"] - g["lent"]
            svg.rect(x, y_of(models_part + g["lent"]), bw, y_of(models_part) - y_of(models_part + g["lent"]),
                     col, 0.55, title="%s: pool lent %.0f GPU-s" % (arm, g["lent"]))
            svg.rect(x, y_of(g["all"]), bw, y_of(models_part + g["lent"]) - y_of(g["all"]),
                     col, 0.25, stroke=col, title="%s: pool held idle %.0f GPU-s" % (arm, held))
        svg.text(x + bw / 2, y_of(g["all"]) - 4, "%.0f" % g["all"], 11, anchor="middle")
        svg.text(x + bw / 2, H - bottom + 18, arm, 12, anchor="middle")
        svg.text(x + bw / 2, H - bottom + 32, "peak %d GPUs" % g["peak"], 10, anchor="middle", fill="#555")
    lx = left + gw * len(arms) + 20
    ly = top + 10
    svg.rect(lx, ly, 12, 12, "#1f77b4"); svg.text(lx + 16, ly + 10, "model replicas", 11)
    svg.rect(lx, ly + 18, 12, 12, "#1f77b4", 0.55); svg.text(lx + 16, ly + 28, "pool, lent", 11)
    svg.rect(lx, ly + 36, 12, 12, "#1f77b4", 0.25, stroke="#1f77b4"); svg.text(lx + 16, ly + 46, "pool, held idle", 11)
    svg.write(os.path.join(out, "gpu-seconds.svg"))


def plot_timeline(data, out):
    arms = [a for a in data["arms"] if data["arms"][a].get("gpu_series")]
    if not arms:
        return
    sched = data["schedule"]
    t_end = sched[-1]["end"]
    gmax = max(max(r[1] for r in data["arms"][a]["gpu_series"]) for a in arms)
    ymax = gmax + 1
    left, right, top, bottom = 60, 20, 50, 60
    W, H = 900, 320
    pw, ph = W - left - right, H - top - bottom
    svg = SVG(W, H)

    def x_of(t):
        return left + max(0.0, min(1.0, t / t_end)) * pw

    def y_of(g):
        return top + ph - (g / ymax) * ph

    svg.text(left, 22, "Accelerators held over the run", 14, weight="bold")
    svg.text(left, 38, "shaded: a model's burst (A above, B below); the bands between are both-low", 11, fill="#555")
    low_a = min(s["rate_a"] for s in sched)
    low_b = min(s["rate_b"] for s in sched)
    for s in sched:
        if s["rate_a"] > low_a:
            svg.rect(x_of(s["start"]), top, x_of(s["end"]) - x_of(s["start"]), ph / 2, "#000", 0.05)
        if s["rate_b"] > low_b:
            svg.rect(x_of(s["start"]), top + ph / 2, x_of(s["end"]) - x_of(s["start"]), ph / 2, "#000", 0.05)
    for g in range(0, int(ymax) + 1):
        y = y_of(g)
        svg.line(left, y, W - right, y, "#e5e5e5")
        svg.text(left - 6, y + 4, "%d" % g, 11, anchor="end")
    svg.text(16, top + ph / 2, "GPUs", 11, anchor="middle", rotate=-90)
    for t in range(0, int(t_end) + 1, 300):
        svg.line(x_of(t), top + ph, x_of(t), top + ph + 4, "#333")
        svg.text(x_of(t), top + ph + 16, "%d" % t, 10, anchor="middle")
    svg.text(left + pw / 2, H - bottom + 34, "seconds since the load started", 11, anchor="middle")
    for arm in arms:
        ser = data["arms"][arm]["gpu_series"]
        pts = []
        prev = None
        for t, total, pool, lent in ser:
            if t < 0 or t > t_end:
                continue
            if prev is not None:
                pts.append((x_of(t), y_of(prev)))
            pts.append((x_of(t), y_of(total)))
            prev = total
        svg.path(pts, COLORS.get(arm, "#999"), 1.8)
    lx = left
    for arm in arms:
        svg.line(lx, H - 14, lx + 20, H - 14, COLORS.get(arm, "#999"), 3)
        svg.text(lx + 26, H - 10, arm, 11)
        lx += 90
    svg.write(os.path.join(out, "fleet-timeline.svg"))


def main(argv):
    p = argparse.ArgumentParser(description=__doc__,
                                formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--json", required=True, help="report.json from two_model_report.py --json")
    p.add_argument("--out", required=True, help="directory to write the SVGs into")
    args = p.parse_args(argv)
    with open(args.json) as fh:
        data = json.load(fh)
    os.makedirs(args.out, exist_ok=True)
    plot_ttft(data, args.out)
    plot_gpu_seconds(data, args.out)
    plot_timeline(data, args.out)
    for f in ("ttft-rises.svg", "gpu-seconds.svg", "fleet-timeline.svg"):
        path = os.path.join(args.out, f)
        if os.path.exists(path):
            print("wrote %s (%d bytes)" % (path, os.path.getsize(path)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
