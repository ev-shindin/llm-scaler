"""Drive a weight transfer where the SENDER is a live serving replica.

Both halves must overlap: the receiver blocks inside /update_weights waiting on
the broadcast, and the sender blocks inside /collective_rpc doing it. So each
gets a thread.

The metadata comes from the SENDER's own parameters, not from a checkpoint --
which is the point of the exercise. A running engine holds FUSED parameters
(qkv_proj, gate_up_proj) where the checkpoint has separate q/k/v, so this also
tests whether the receiver's load_weights accepts what a live engine actually has.
"""
import json
import os
import sys
import threading
import time
import traceback

import requests

SENDER = os.environ["SENDER"]      # http://ip:8000 of the serving replica
RECEIVER = os.environ["RECEIVER"]  # http://ip:8000 of the engine being filled
MASTER = os.environ["MASTER"]      # address the receiver can reach the sender on
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 29800

# The transfer group is ONE sender rank plus every receiver rank: the sender
# gathers across its own TP and broadcasts full tensors, and each receiver rank
# shards what it gets on load. So the sender's TP never enters this number, and
# the receiver's always does -- a TP=2 receiver makes a group of three, not two.
RECV_TP = int(os.environ.get("RECV_TP", "1"))
WORLD = 1 + RECV_TP


def post(base, path, payload=None, timeout=900):
    r = requests.post("%s/%s" % (base, path), json=payload, timeout=timeout)
    tag = "sender" if base == SENDER else "recv  "
    print("  %s %-28s -> %s %s" % (tag, path, r.status_code, r.text[:200]), flush=True)
    r.raise_for_status()
    return r


meta = post(SENDER, "collective_rpc", {"method": "weight_metadata"}).json()["results"][0]
names = [m[0] for m in meta]
print("sender holds %d parameters (fused: %d); transfer group of %d (1 sender + %d receiver rank(s))"
      % (len(names), sum(1 for n in names if "qkv" in n or "gate_up" in n), WORLD, RECV_TP), flush=True)

init_info = {"master_address": MASTER, "master_port": PORT,
             "rank_offset": 1, "world_size": WORLD}
update_info = {"names": names,
               "dtype_names": [m[1] for m in meta],
               "shapes": [m[2] for m in meta],
               "packed": False}

err = {}


def drive_receiver():
    try:
        post(RECEIVER, "init_weight_transfer_engine", {"init_info": init_info})
        post(RECEIVER, "start_weight_update")
        t0 = time.time()
        post(RECEIVER, "update_weights", {"update_info": update_info})
        print("  receiver update_weights returned in %.2fs" % (time.time() - t0), flush=True)
        post(RECEIVER, "finish_weight_update")
    except Exception as e:
        err["recv"] = traceback.format_exc()
        print("RECEIVER FAILED: %s" % e, flush=True)


t = threading.Thread(target=drive_receiver, daemon=True)
t.start()
time.sleep(1)

try:
    t0 = time.time()
    r = post(SENDER, "collective_rpc",
             {"method": "send_weights_to",
              "kwargs": {"master_address": MASTER, "master_port": PORT, "world_size": WORLD}})
    print("  sender broadcast returned in %.2fs" % (time.time() - t0), flush=True)
except Exception as e:
    err["send"] = traceback.format_exc()
    print("SENDER FAILED: %s" % e, flush=True)

t.join(timeout=900)
print("errors:", {k: v.splitlines()[-1] for k, v in err.items()} or "none", flush=True)
