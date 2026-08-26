"""Worker extension letting a SERVING replica donate its weights, at any TP.

The shipped protocol is one UNSHARDED sender (always rank 0) broadcasting to N
inference ranks, each of which shards on load. A replica running tensor
parallelism holds only its own shard, so it cannot be that sender as-is.

It can become one. Every parameter a parallel layer creates is tagged with the
dimension it was split along -- `output_dim` for column-parallel, `input_dim` for
row-parallel, neither for replicated -- so the shards can be gathered back into
the checkpoint-shaped tensor the receiver expects, one parameter at a time.

That keeps the sender's TP independent of the receiver's, which rank-pairing
would not.
"""


class WeightDonor:
    def _tp_group(self):
        from vllm.distributed.parallel_state import get_tp_group
        return get_tp_group()

    def _full(self, param):
        """This parameter as the checkpoint holds it, gathered across TP ranks."""
        import torch
        group = self._tp_group()
        if group.world_size == 1:
            return param.data
        dim = getattr(param, "output_dim", None)
        if dim is None:
            dim = getattr(param, "input_dim", None)
        if dim is None:
            # Untagged means replicated: every rank has the whole thing, and
            # gathering would concatenate duplicates.
            return param.data
        gathered = group.all_gather(param.data.contiguous(), dim=dim)
        return gathered

    def weight_metadata(self):
        model = self.model_runner.model
        out = []
        for name, p in model.named_parameters():
            full = self._full(p)
            out.append((name, str(full.dtype).replace("torch.", ""), list(full.shape)))
        return out

    def send_weights_to(self, master_address, master_port, world_size=2):
        from vllm.distributed.weight_transfer.nccl_common import trainer_init
        from vllm.distributed.weight_transfer.nccl_engine import (
            NCCLTrainerSendWeightsArgs,
            NCCLWeightTransferEngine,
        )

        model = self.model_runner.model
        group = self._tp_group()

        # Only ONE process may be rank 0 of the transfer group. Every TP rank has
        # to run the gather, though -- it is a collective, and a rank that skips
        # it hangs the others.
        pairs = [(n, self._full(p)) for n, p in model.named_parameters()]
        if group.rank_in_group != 0:
            return 0

        comm = trainer_init({
            "master_address": master_address,
            "master_port": int(master_port),
            "world_size": int(world_size),
        })
        NCCLWeightTransferEngine.trainer_send_weights(
            iter(pairs),
            NCCLTrainerSendWeightsArgs(group=comm, src=0, packed=False),
        )
        return len(pairs)
