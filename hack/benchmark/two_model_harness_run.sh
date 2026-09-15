#!/bin/sh
# Runs ONE model's inference-perf profile inside the load Pod.
#
# Both models are driven by containers of the same Pod running this script,
# with mirrored stage ladders, so the scenario's anti-phase only holds if the
# two containers cross their stage boundaries together. Three things here make
# that true, and each exists because the obvious version does not:
#
#   1. The tokenizer is fetched BEFORE the barrier. inference-perf downloads it
#      on startup, and the two models are different sizes from different repos,
#      so the downloads do not take the same time. Fetching after the barrier
#      would offset the ladders by that difference for the whole run.
#   2. The barrier is an ABSOLUTE epoch handed in by the driver, not a sleep.
#      A sleep measures from whenever the container happened to start, which is
#      exactly the skew being removed.
#   3. Overrunning the barrier is announced (LATE-START) rather than absorbed.
#      A container that reaches the barrier after it has passed starts its
#      first stage late and stays late; that run is not anti-phase and the
#      driver refuses it rather than reporting it.
#
# The Pod is held open after the run. `kubectl cp` is exec+tar and cannot run
# in a terminated container, so exiting here would discard the results of every
# successful run. The driver deletes the Job once it has copied.

: "${ROLE:?ROLE must be a or b}"
: "${TOKENIZER:?TOKENIZER must be the model id to tokenize with}"
: "${MODEL:?MODEL must be the served model id}"
: "${BASE_URL:?BASE_URL must be this stack's endpoint, with no route appended}"
: "${START_AT:?START_AT must be the absolute epoch both containers start at}"
: "${PROFILE:=/profiles/profile-${ROLE}.yaml}"
: "${HOLD_SECONDS:=3600}"

export HF_HOME="${HF_HOME:-/hf}"
export HOME="${HOME:-/hf}"
mkdir -p "$HF_HOME" || true

echo "role=$ROLE tokenizer=$TOKENIZER profile=$PROFILE start_at=$START_AT"

# Pre-fetching is not an optimisation here, it is what keeps the two ladders in
# phase. It also fails LOUDLY and early: a tokenizer that cannot be fetched
# fails inference-perf at startup, minutes into a run that has already begun
# holding accelerators.
#
# RETRIED, because the fetch crosses the public internet and a blip there costs
# a whole arm. Measured: `CAS Client Error: Request middleware error: error
# sending request` from the Hugging Face CDN took model A down while model B
# fetched fine, and the arm was refused after five minutes of preload grace --
# with both models' accelerators already held.
python3 - "$TOKENIZER" <<'PY'
import sys, time
from transformers import AutoTokenizer

name = sys.argv[1]
for attempt in range(1, 6):
    try:
        AutoTokenizer.from_pretrained(name)
        print("tokenizer cached: %s (attempt %d)" % (name, attempt), flush=True)
        sys.exit(0)
    except Exception as exc:          # noqa: BLE001 -- any failure is worth a retry
        # Printed every time: a fetch that needed four attempts is worth knowing
        # about even when the fifth succeeds.
        print("tokenizer fetch %d/5 failed for %s: %s: %s"
              % (attempt, name, type(exc).__name__, str(exc)[:200]), flush=True)
        time.sleep(5 * attempt)
print("TOKENIZER-UNFETCHABLE %s" % name, flush=True)
sys.exit(1)
PY
preload_rc=$?
if [ "$preload_rc" -ne 0 ]; then
    echo "PRELOAD-FAILED-$ROLE rc=$preload_rc"
    sleep "$HOLD_SECONDS"
    exit "$preload_rc"
fi

# The endpoint has to ANSWER before the barrier, not merely exist. Measured on
# CoreWeave: 12 of 210 requests -- 6%, three times the report's client-side loss
# threshold -- failed with ClientConnectorDNSError, "Temporary failure in name
# resolution", because this Pod's istio sidecar was not ready when the app
# container started issuing. Those are the DRIVER's failures; a run thinned by
# them measures the survivors, not the scenario.
python3 - "$BASE_URL" "$MODEL" "$ROLE" <<'PY'
import json, sys, time, urllib.error, urllib.request
base, model, role = sys.argv[1], sys.argv[2], sys.argv[3]
body = json.dumps({"model": model, "prompt": "ping", "max_tokens": 1}).encode()
deadline, last = time.time() + 300, None
while time.time() < deadline:
    try:
        req = urllib.request.Request(base.rstrip("/") + "/v1/completions", data=body,
                                     headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=30) as resp:
            if resp.status == 200:
                print("endpoint answers: %s" % base, flush=True)
                sys.exit(0)
            last = "HTTP %s" % resp.status
    except Exception as exc:          # noqa: BLE001 -- any failure means not ready
        last = "%s: %s" % (type(exc).__name__, exc)
    time.sleep(3)
print("ENDPOINT-UNREACHABLE-%s %s (%s)" % (role, base, last), flush=True)
sys.exit(1)
PY
probe_rc=$?
if [ "$probe_rc" -ne 0 ]; then
    echo "PRELOAD-FAILED-$ROLE rc=$probe_rc"
    sleep "$HOLD_SECONDS"
    exit "$probe_rc"
fi
echo "PRELOAD-DONE-$ROLE"

python3 - "$START_AT" "$ROLE" <<'PY'
import sys, time
target, role = float(sys.argv[1]), sys.argv[2]
delta = target - time.time()
if delta < 0:
    # Announced, not absorbed: this container will run its whole ladder offset
    # from the other one's by |delta|, which is not the scenario.
    print("LATE-START-%s by %.1fs" % (role, -delta), flush=True)
else:
    print("barrier: %s waits %.1fs" % (role, delta), flush=True)
    time.sleep(delta)
PY

inference-perf --config_file "$PROFILE"
rc=$?
echo "inference-perf exited $rc"
if [ "$rc" -ne 0 ]; then
    echo "RUN-FAILED-$ROLE rc=$rc"
else
    # The line the driver waits for, printed only after inference-perf has
    # returned and therefore after its report files are on disk.
    echo "RESULTS-WRITTEN-$ROLE"
fi
echo "holding the Pod open for collection"
sleep "$HOLD_SECONDS"
exit "$rc"
