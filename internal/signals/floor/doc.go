// Package floor is the throughput floor: the demand the offered load implies
// per role once each role's saturated completion rate is known. What it
// prices, why, and what the alternatives cost, measured:
//
// Occupancy -- resident KV plus the waiting queues -- is a state of the
// fleet, not a property of the load, and it falls as replicas are added. Any
// floor built from a per-request cost measured on the current fleet has the
// same defect: the service time the engines report is ITL x output length,
// and ITL grows with the batch a replica is running. Measured on one decode
// pod across a run at a constant 6 req/s and a constant 6000/1000 shape
// (biran-20260915-102548-571):
//
//	replicas sharing the load   batch   ITL      W (service time)
//	6                           ~1      2.0 ms   2.0 s
//	1                           136-178 18-50 ms 16-52 s
//
// A 25x range in W at the same load, so lambda x W x tokens with the W of the
// moment is current occupancy restated. A floor built that way (the
// arrival-rate floor this package replaced) read 88k tokens at six replicas and
// authorised the scale-down to one; and on the way UP it did the opposite
// harm, pricing the load at the inflated W of a fleet that was behind,
// ordering replicas before any had reached saturation, and releasing them
// once they had brought W back down. Measured as a 2-4 replica oscillation
// with a ten-minute period on the second phase of the shape-swap benchmark
// (docs/proposals/backlog-sizing.md).
//
// What a replica can do does not move with the fleet: its completion rate
// when saturated. With the queue over the threshold the engine is completing
// requests as fast as it can for this shape, and that rate -- recorded beside
// k2 at the same P1 moment, under the same history key -- is a per-replica
// THROUGHPUT, mu. The fleet then needs lambda / mu replicas to keep up, which
// in the saturation analyzer's (D, P) contract is a demand of (lambda / mu) x P tokens.
// On the run above: mu read 5.4 req/s at the 6000/1000 shape and 2.5 req/s at
// 1000/4000, against 6 req/s arriving -- 2 and 3 decode replicas, which is
// where the fleet in fact held when it was left alone at those sizes.
//
// Strictly a floor: it never lowers demand. It is not capped at the fleet's
// own size. The first version was, so that it could hold a fleet but never
// order one, and scale-up stayed with occupancy and the queues. Measured, that
// left the order too late: a lone replica at 6 req/s against a mu of 5.4-6.4
// is at 94% of what it can do from the first cycle, and it tips into
// preemption at 75-145 s; occupancy crossed k1 at +69-78 s on every run,
// which with a 60-100 s start lands the second replica after the tip on most
// of them. The floor's lambda / mu = 0.94 replicas, through the engine's
// scale-up headroom, orders it in the first cycle lambda is measured -- some
// 50 s earlier -- and the bound on what it can order is
// (lambda + backlog / drain) / mu by construction, a figure that does not
// move as replicas are added. What a mu that under-read costs is bounded by
// the under-read (the window keeps a max, so it corrects upward at the next
// saturation and never drifts down); what a late order cost was five to
// seven replicas at the first ramp of every run.
//
// Two readings are not trusted with an order, only with a hold. A mu BORROWED from a
// neighbouring bucket (the saturation analyzer's
// nearestSaturatedThroughput) is wrong in a known
// direction and, from a longer shape, over-orders: the old cap at scaleUp x
// anticipated supply stays, a hold and no more. A window with a SINGLE
// reading is the first window's under-read -- an order on it
// over-provisions, and the over-provisioned fleet never saturates again to
// record the second reading that would have corrected it
// (MinThroughputSamplesToOrder) -- the cap stays, a hold and no more. The
// second reading is a rate window away (ThroughputSampleSpacing), and a
// fleet held at its size for that minute, its queues priced nowhere, lands
// the cold ramp's next replica 15-45 s after the old dead guard would have
// (occupancy orders in the meantime, or the second reading does); letting
// the one reading order one replica was tried and dropped, see
// Estimate.
//
// The same model prices a BACKLOG. Occupancy charged every queued request at
// its full KV footprint, as if all of them had to be resident at once, and
// the engine sized the fleet to hold them: 350 queued requests became five to
// seven extra replicas, each arriving after the queue was gone. A backlog
// needs throughput, not simultaneous residency: B requests to be cleared
// within T seconds on top of lambda arriving is (lambda + B / T) / mu
// replicas. For a role whose mu is known, the residency charge on its queues
// (the engines' own and the scheduler's) is taken back out and the queued
// requests enter the floor as B / T instead; the resident KV term stays as
// measured. A role with no mu keeps the residency charge, which is at least
// an opinion -- except prefill's share of the scheduler queue, which is
// dropped: a prefill replica holds a prompt's KV for its prefill time plus
// the hand-off to decode, and the only thing that makes it hold more is
// decode being saturated, which more prefill replicas do not fix. Measured:
// every prefill replica beyond the first at the first ramp was ordered by
// that term, and none of them prefilled anything a single one could not.
package floor
