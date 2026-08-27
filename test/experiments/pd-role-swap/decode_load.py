"""Decode-shaped load: many concurrent short prompts, long generations.

That is the shape a decode engine is tuned for -- small batches of tokens per
step, many sequences alive at once -- so it is what distinguishes an engine that
is genuinely decode-shaped from one merely told to be.
"""
import json
import sys
import threading
import time
import urllib.request

URL = "http://127.0.0.1:8000/v1/completions"
CONC = int(sys.argv[1]) if len(sys.argv) > 1 else 16
GEN = int(sys.argv[2]) if len(sys.argv) > 2 else 128
lat, lock = [], threading.Lock()


def one(i):
    body = json.dumps({
        "model": "Qwen/Qwen3-8B",
        "prompt": "Count slowly and describe each number you say, starting from %d:" % i,
        "max_tokens": GEN, "temperature": 0, "seed": i,
    }).encode()
    t0 = time.time()
    req = urllib.request.Request(URL, data=body, headers={"Content-Type": "application/json"})
    r = json.load(urllib.request.urlopen(req, timeout=600))
    dt = time.time() - t0
    with lock:
        lat.append((dt, r["usage"]["completion_tokens"]))


start = time.time()
threads = [threading.Thread(target=one, args=(i,)) for i in range(CONC)]
for t in threads:
    t.start()
for t in threads:
    t.join()
wall = time.time() - start

toks = sum(n for _, n in lat)
lat.sort()
print("  concurrency %d, %d requests" % (CONC, len(lat)))
print("  wall           %.2f s" % wall)
print("  output tokens  %d" % toks)
print("  throughput     %.1f tok/s" % (toks / wall))
print("  latency p50    %.2f s   p95 %.2f s" % (lat[len(lat) // 2][0], lat[int(len(lat) * 0.95)][0]))
