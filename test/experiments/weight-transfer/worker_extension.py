"""Worker extension that lets a SERVING vLLM replica donate its own weights.

vLLM's weight-transfer engine is receiver-side over HTTP; the sender exists only
as a Python API. `worker_extension_cls` is the supported way in: methods defined
here are inherited by the worker class and callable through /collective_rpc.

The open question this is written to answer: a running engine holds FUSED
parameters (qkv_proj, gate_up_proj), while the receiver's load_weights expects
CHECKPOINT-format names. If those disagree, a live replica cannot donate its
weights without un-fusing them, and the whole "sender is a replica you already
have" story needs rethinking.
"""


class WeightDonor:
    def weight_metadata(self):
        """(name, dtype, shape) for every parameter this worker holds."""
        model = self.model_runner.model
        return [
            (name, str(p.dtype).replace("torch.", ""), list(p.shape))
            for name, p in model.named_parameters()
        ]

    def send_weights_to(self, master_address, master_port, world_size=2):
        """Form an NCCL group as rank 0 and broadcast our parameters into it."""
        from vllm.distributed.weight_transfer.nccl_common import trainer_init
        from vllm.distributed.weight_transfer.nccl_engine import (
            NCCLTrainerSendWeightsArgs,
            NCCLWeightTransferEngine,
        )

        group = trainer_init({
            "master_address": master_address,
            "master_port": int(master_port),
            "world_size": int(world_size),
        })
        model = self.model_runner.model
        names = [n for n, _ in model.named_parameters()]
        NCCLWeightTransferEngine.trainer_send_weights(
            ((n, p) for n, p in model.named_parameters()),
            NCCLTrainerSendWeightsArgs(group=group, src=0, packed=False),
        )
        return len(names)
