"""Prototype of the P/D role swap: change the scheduler's budget at run time.

Scoping question this answers: how much of vLLM must a fork touch to turn a
prefill-shaped engine into a decode-shaped one and back, without moving weights
or reallocating KV?

The scheduler lives in the EngineCore PROCESS, which /collective_rpc cannot
reach -- that route dispatches to workers. So rather than add a route (which a
real change would), this intercepts collective_rpc on EngineCore and handles one
reserved method name itself. Same reachability, no new HTTP surface, and it
keeps the prototype to one file.

The bound is the point. max_num_batched_tokens sizes the model runner's input
buffers, the torch.compile specialisation range, the encoder cache and the
attention workspace -- all at init, all from the LAUNCH value. Lowering is safe
because every one of them is then oversized; raising past the launch value is
not, and is refused here.
"""
import os

_LAUNCH = {}


def _install():
    from vllm.v1.engine.core import EngineCore

    original = EngineCore.collective_rpc

    def collective_rpc(self, method, timeout=None, args=(), kwargs=None):
        if method != "__budget":
            return original(self, method, timeout, args, kwargs)

        kw = dict(kwargs or {})
        sched = self.scheduler
        cfg = self.vllm_config.scheduler_config

        # Remember what the engine STARTED with: that is the ceiling every
        # derived structure was sized against.
        if not _LAUNCH:
            _LAUNCH["tokens"] = cfg.max_num_batched_tokens
            _LAUNCH["seqs"] = cfg.max_num_seqs

        report = {
            "launch_max_num_batched_tokens": _LAUNCH["tokens"],
            "launch_max_num_seqs": _LAUNCH["seqs"],
        }

        want_tokens = kw.get("max_num_batched_tokens")
        if want_tokens is not None:
            want_tokens = int(want_tokens)
            if want_tokens > _LAUNCH["tokens"]:
                report["refused"] = (
                    "max_num_batched_tokens %d exceeds the launch value %d; the input "
                    "buffers, compile ranges and attention workspace were sized for the "
                    "launch value and cannot grow" % (want_tokens, _LAUNCH["tokens"])
                )
            else:
                cfg.max_num_batched_tokens = want_tokens
                sched.max_num_scheduled_tokens = want_tokens

        want_seqs = kw.get("max_num_seqs")
        if want_seqs is not None:
            want_seqs = int(want_seqs)
            if want_seqs > _LAUNCH["seqs"]:
                report["refused_seqs"] = (
                    "max_num_seqs %d exceeds the launch value %d" % (want_seqs, _LAUNCH["seqs"])
                )
            else:
                cfg.max_num_seqs = want_seqs
                sched.max_num_running_reqs = want_seqs

        report["now_max_num_batched_tokens"] = cfg.max_num_batched_tokens
        report["now_max_num_seqs"] = cfg.max_num_seqs
        report["scheduler_max_num_scheduled_tokens"] = sched.max_num_scheduled_tokens
        report["scheduler_max_num_running_reqs"] = sched.max_num_running_reqs
        return [report]

    EngineCore.collective_rpc = collective_rpc


if os.environ.get("VLLM_BUDGET_PATCH") == "1":
    try:
        _install()
    except Exception as exc:  # a failed patch must not stop the engine starting
        print("budget patch NOT installed: %r" % (exc,), flush=True)
