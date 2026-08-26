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

## What this does NOT show

Two GPUs in one Pod, TP=1, 1.4 GiB. It says nothing yet about inter-node RoCE,
about a live serving replica acting as the sender, about TP>1 where each rank
needs its own shard, or about a model 500x this size.
