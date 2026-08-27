"""Prefill-heavy load: long prompts, few output tokens.

This is where max_num_batched_tokens binds. At 8192 a long prompt is prefilled
in one or two steps; at 2048 the same prompt is chunked into more. If the swap
is real, the two budgets must produce measurably different prefill throughput --
otherwise the setting was recorded and not honoured.
"""
import json, sys, threading, time, urllib.request
CONC = int(sys.argv[1]) if len(sys.argv) > 1 else 8
WORDS = int(sys.argv[2]) if len(sys.argv) > 2 else 700
lat, lock = [], threading.Lock()
PROMPT = ("The quick brown fox jumps over the lazy dog. " * WORDS) + " Summarise in one word:"

def one(i):
    body = json.dumps({"model": "Qwen/Qwen3-8B", "prompt": PROMPT,
                       "max_tokens": 4, "temperature": 0, "seed": i}).encode()
    t0 = time.time()
    req = urllib.request.Request("http://127.0.0.1:8000/v1/completions", data=body,
                                 headers={"Content-Type": "application/json"})
    r = json.load(urllib.request.urlopen(req, timeout=900))
    with lock:
        lat.append((time.time() - t0, r["usage"]["prompt_tokens"]))

t0 = time.time()
ts = [threading.Thread(target=one, args=(i,)) for i in range(CONC)]
for t in ts: t.start()
for t in ts: t.join()
wall = time.time() - t0
ptok = sum(n for _, n in lat)
lat.sort()
print("  %d prompts x %d prompt-tokens" % (len(lat), lat[0][1]))
print("  wall            %.2f s" % wall)
print("  PREFILL tok/s   %.0f" % (ptok / wall))
print("  latency p50     %.2f s" % lat[len(lat)//2][0])
