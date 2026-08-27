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

    def _shard_dims(self):
        """param name -> the dimension tensor parallelism split it along.

        NOT readable from the parameter's attributes: vLLM sets BOTH
        `output_dim` and `input_dim` on many weights (see
        `set_weight_attrs(weight, {"input_dim": 1, "output_dim": 0})` in
        vocab_parallel_embedding.py), so presence disambiguates nothing. Reading
        `output_dim` first gathers row-parallel weights along the wrong axis and
        yields [8192, 2048] where the checkpoint has [4096, 4096] -- measured.

        The layer CLASS is what decides it: row-parallel splits the input, every
        column-ish layer splits the output. A row-parallel bias is replicated.
        """
        dims = {}
        for mod_name, mod in self.model_runner.model.named_modules():
            cls = type(mod).__name__
            if "RowParallel" in cls:
                sharded = {"weight": 1}
            elif any(k in cls for k in ("ColumnParallel", "QKVParallel",
                                        "MergedColumn", "VocabParallel",
                                        "ParallelLMHead")):
                sharded = {"weight": 0, "bias": 0}
            else:
                continue
            for p_name, _ in mod.named_parameters(recurse=False):
                if p_name in sharded:
                    dims["%s.%s" % (mod_name, p_name) if mod_name else p_name] = sharded[p_name]
        return dims

    def _full(self, name, param, dims):
        """This parameter as the checkpoint holds it, gathered across TP ranks."""
        group = self._tp_group()
        if group.world_size == 1:
            return param.data
        dim = dims.get(name)
        if dim is None:
            # Replicated: every rank has the whole thing, and gathering would
            # concatenate duplicates.
            return param.data
        return group.all_gather(param.data.contiguous(), dim=dim)

    def weight_metadata(self):
        model = self.model_runner.model
        dims = self._shard_dims()
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
        dims = self._shard_dims()
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
