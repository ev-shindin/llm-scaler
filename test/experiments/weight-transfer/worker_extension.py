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

    def _shard_plan(self):
        """param name -> (shard axis, per-constituent partition sizes).

        Two things must be right, and only the first is visible in the shape.

        AXIS is not readable from the parameter's attributes: vLLM sets BOTH
        `output_dim` and `input_dim` on many weights, so presence disambiguates
        nothing. The layer CLASS decides -- row-parallel splits the input, every
        column-ish layer splits the output, a row-parallel bias is replicated.

        LAYOUT is the one that bites silently. A MERGED layer (qkv_proj,
        gate_up_proj) holds its constituents CONCATENATED within each rank's
        shard: rank i has [q_i ; k_i ; v_i]. Gathering naively produces
        [q0;k0;v0;q1;k1;v1] where the checkpoint has [q0;q1;k0;k1;v0;v1] -- the
        SAME SHAPE and different content. Measured: a TP=2 -> TP=1 transfer
        built that way reported success at every step and served
        'REMABCDEFGHIscp...'.

        `output_partition_sizes` is the per-constituent size within one rank's
        shard, which is what makes the pieces separable.
        """
        plan = {}
        for mod_name, mod in self.model_runner.model.named_modules():
            cls = type(mod).__name__
            if "RowParallel" in cls:
                sharded, axis = {"weight"}, 1
            elif any(k in cls for k in ("ColumnParallel", "QKVParallel",
                                        "MergedColumn", "VocabParallel",
                                        "ParallelLMHead")):
                sharded, axis = {"weight", "bias"}, 0
            else:
                continue
            parts = getattr(mod, "output_partition_sizes", None)
            if axis != 0 or not parts or len(parts) < 2:
                parts = None
            for p_name, _ in mod.named_parameters(recurse=False):
                if p_name in sharded:
                    key = "%s.%s" % (mod_name, p_name) if mod_name else p_name
                    plan[key] = (axis, parts)
        return plan

    def _full(self, name, param, plan):
        """This parameter as the checkpoint holds it, gathered across TP ranks."""
        import torch
        group = self._tp_group()
        if group.world_size == 1:
            return param.data
        entry = plan.get(name)
        if entry is None:
            # Replicated: every rank has the whole thing.
            return param.data
        axis, parts = entry
        data = param.data.contiguous()
        if parts is None:
            return group.all_gather(data, dim=axis)
        # Merged layer: separate the constituents, gather each, then lay them
        # out in checkpoint order.
        offset, gathered = 0, []
        for size in parts:
            piece = data.narrow(axis, offset, size).contiguous()
            gathered.append(group.all_gather(piece, dim=axis))
            offset += size
        return torch.cat(gathered, dim=axis)

    def weight_metadata(self):
        model = self.model_runner.model
        dims = self._shard_plan()
        out = []
        for name, p in model.named_parameters():
            full = self._full(name, p, dims)
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
        dims = self._shard_plan()
        pairs = [(n, self._full(n, p, dims)) for n, p in model.named_parameters()]
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
