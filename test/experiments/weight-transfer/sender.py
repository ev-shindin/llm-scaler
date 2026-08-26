"""Fill an L2-slept vLLM engine with weights pushed from another GPU.

A level-2 sleeper is a live, compiled engine holding no host RAM and garbage
where its weights were -- structurally, a receiver waiting for a broadcast. This
is the sender half, driving both ends by hand because v0.26.0 registers no
trainer-side engines with WeightTransferTrainerFactory.

The two halves MUST overlap: the receiver blocks inside /update_weights waiting
on the NCCL broadcast, so the HTTP calls run on a thread while the broadcast
runs here.
"""
import glob
import json
import os
import sys
import threading
import time
import traceback

import requests
import torch
from safetensors.torch import load_file

from vllm.distributed.weight_transfer.nccl_common import trainer_init
from vllm.distributed.weight_transfer.nccl_engine import (
    NCCLTrainerSendWeightsArgs,
    NCCLWeightTransferEngine,
)

MODEL = "/model-cache/models/Qwen/Qwen3-0.6B"
BASE = "http://127.0.0.1:8000"
PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 29600


def post(path, payload=None):
    r = requests.post("%s/%s" % (BASE, path), json=payload, timeout=600)
    print("  HTTP %-28s -> %s %s" % (path, r.status_code, r.text[:160]), flush=True)
    r.raise_for_status()


def main():
    tensors = {}
    for f in sorted(glob.glob(os.path.join(MODEL, "*.safetensors"))):
        for name, t in load_file(f).items():
            tensors[name] = t.cuda()
    order = sorted(tensors)
    total = sum(tensors[n].numel() * tensors[n].element_size() for n in order)
    print("loaded %d tensors, %.2f GiB" % (len(order), total / 2**30), flush=True)

    init_info = {"master_address": "127.0.0.1", "master_port": PORT,
                 "rank_offset": 1, "world_size": 2}
    update_info = {
        "names": order,
        "dtype_names": [str(tensors[n].dtype).replace("torch.", "") for n in order],
        "shapes": [list(tensors[n].shape) for n in order],
        "packed": False,
    }

    err = {}

    def drive():
        # The receiver's side of the rendezvous, then the round trip. Runs
        # concurrently with the broadcast below by necessity, not by choice.
        try:
            post("init_weight_transfer_engine", {"init_info": init_info})
            post("start_weight_update")
            post("update_weights", {"update_info": update_info})
            post("finish_weight_update")
        except Exception as e:
            err["drive"] = traceback.format_exc()
            print("DRIVE FAILED: %s" % e, flush=True)

    t = threading.Thread(target=drive, daemon=True)
    t.start()

    print("rendezvous as rank 0 on port %d ..." % PORT, flush=True)
    t0 = time.time()
    group = trainer_init(init_info)
    print("rendezvous ok in %.1fs" % (time.time() - t0), flush=True)

    t0 = time.time()
    NCCLWeightTransferEngine.trainer_send_weights(
        ((n, tensors[n]) for n in order),
        NCCLTrainerSendWeightsArgs(group=group, src=0, packed=False),
    )
    torch.cuda.synchronize()
    dt = time.time() - t0
    print("BROADCAST %.2f GiB in %.2fs (%.2f GB/s)"
          % (total / 2**30, dt, total / 1e9 / max(dt, 1e-9)), flush=True)

    t.join(timeout=300)
    print("drive errors:", err or "none", flush=True)


if __name__ == "__main__":
    main()
