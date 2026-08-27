# Refilling a level-2 sleeper from a peer

The experiment behind `docs/proposals/warm-pool-weight-transfer.md` §4c. It
answers: can an engine whose weights were DISCARDED by a level-2 sleep be
refilled over NCCL from another GPU, instead of re-read from storage?

**Answered yes on 2026-08-26.** 1.40 GiB in 0.11 s (13.1 GB/s) against 999 ms
for the storage reload of the same model on the same engine, and the post-transfer
output was byte-identical to the pre-sleep output.

## Running it

```bash
kubectl apply -f receiver-pod.yaml          # 2 GPUs: vLLM on 0, sender on 1
kubectl cp sender.py <ns>/xfer:/scripts/sender.py
kubectl exec xfer -- curl -sX POST 'localhost:8000/sleep?level=2'
kubectl exec xfer -- curl -sX POST 'localhost:8000/wake_up'
# the engine now serves '!!!!!!!!!!!!' -- that is the state being rescued
kubectl exec xfer -- bash -lc 'cd /scripts && CUDA_VISIBLE_DEVICES=1 python3 sender.py 29610'
```

## Three things that are load-bearing

- **`--weight-transfer-config '{"backend":"nccl"}'` on the receiver.** A startup
  flag. Without it every weight route answers "Weight transfer not configured",
  so it cannot be enabled at the moment a transfer is first wanted.
- **The sender cannot use `WeightTransferTrainerFactory`.** It registers no
  engines in v0.26.0 and raises "Available trainer engines: []". Drive
  `nccl_common.trainer_init` and `NCCLWeightTransferEngine.trainer_send_weights`
  directly, as `sender.py` does.
- **The halves must overlap.** The receiver blocks inside `/update_weights`
  waiting on the broadcast. Driving the HTTP calls and the broadcast in sequence
  deadlocks; `sender.py` runs the HTTP side on a thread for exactly this reason.

Also: on OpenShift set `USER` and `HOME`, or torch's `cache_dir` calls
`getpass.getuser()` and `import vllm` dies with `KeyError: getpwuid()`.

## TP=2 to TP=2, across nodes

Measured 2026-08-27 on pokprod: Qwen3-8B (bf16, ~16.4 GB) donated by a serving
TP=2 replica to a TP=2 receiver whose weights a level-2 sleep had discarded.
Output identical on three prompts, including generative ones, not just
"capital of France".

    transfer group of 3 (1 sender + 2 receiver ranks)
    291 parameters, 72 of them fused
    receiver update_weights returned in 14.67s

The group size is the thing to get right: the protocol is ONE sender rank plus
EVERY receiver rank, so a TP=2 receiver makes a group of three. The sender's own
TP never enters it -- `WeightDonor` gathers across the sender's ranks and
broadcasts full unsharded tensors (`qkv_proj [6144, 4096]`, not a half), and each
receiver rank shards on load. That is why sender TP and receiver TP need not
match, and why the TP=2->TP=2 case needed no sharding change at all -- only
`RECV_TP` on the orchestrator.

14.67 s against ~37 s for the same model off this cluster's PVC. Both numbers are
LOWER BOUNDS on what the mechanism can do: ~16.4 GB in 14.67 s is ~1.1 GB/s,
which is an ordinary pod network and not RoCE. The ratio is what transfers to
better hardware, not the seconds.

## What this does NOT show

Nothing here covers RoCE or NVLink between the two Pods, a receiver whose TP
DIFFERS from the sender's under real traffic, or a model 100x this size -- where
the sender's gather, which materialises each full parameter on rank 0, is the
step that has to be re-examined.

## A live replica as the sender

`worker_extension.py` + `orchestrator.py` do the same transfer with no
checkpoint on the sender: a SERVING replica donates its own parameters.

```bash
kubectl create configmap wext --from-file=wext.py=worker_extension.py
# sender:   --worker-extension-cls wext.WeightDonor   (PYTHONPATH=/ext, configMap at /ext)
# receiver: --enable-sleep-mode --weight-transfer-config '{"backend":"nccl"}'
SENDER=http://127.0.0.1:8000 RECEIVER=http://<recv-ip>:8000 MASTER=<sender-ip> \
  python3 orchestrator.py 29810
```

Measured 2026-08-26 across two nodes: byte-identical output, 291 parameters of
which 72 are FUSED (`qkv_proj`, `gate_up_proj`) against the checkpoint's 399
separate ones. The receiver accepts the fused layout because its own parameters
carry those names — **but only while both sides run the same vLLM version and
parallelism.** Fusion layout is an internal detail, not a wire format.

**The sender returns early.** `/collective_rpc` came back in 0.77 s while the
receiver's `/update_weights` took 14.78 s. Do not gate readiness on the sender.
