// Package floor is the throughput floor: the demand the offered load implies
// per role once each role's saturated completion rate is known.
//
// Occupancy -- resident KV plus the waiting queues -- is a state of the fleet
// rather than a property of the load, and it falls as replicas are added, so
// a fleet that is keeping up reads as one that needs fewer replicas. What a
// replica can do does not move with the fleet: its completion rate when
// saturated, mu, recorded beside k2 at the same moment and under the same
// history key. A fleet then needs lambda / mu replicas to keep up, which in
// the saturation analyzer's (D, P) contract is a demand of
// (lambda / mu) x P tokens.
//
// Three properties, each of which cost a measured mistake to establish, and
// each of which is recorded with its run in
// docs/developer-guide/analyzer-evidence.md:
//
//   - The floor only ever raises demand, and is not capped at the fleet's own
//     size. It does not need to be: what it can order is
//     (lambda + backlog / drain) / mu by construction, a figure fixed by the
//     load and the per-replica rate, which does not move as replicas are
//     added. Capped, it held a fleet but never ordered one, and the order
//     arrived after the lone replica had already tipped into preemption.
//   - A reading the fleet has not earned may hold the fleet but not grow it:
//     one borrowed from a neighbouring output-length bucket, or a window
//     holding fewer than MinThroughputSamplesToOrder readings, is capped at
//     the role's anticipated supply. A borrowed reading never outvotes a
//     replica's own.
//   - A backlog is priced as work to drain -- B requests within
//     BacklogDrainSeconds on top of lambda arriving -- not as simultaneous
//     residency, which sized fleets to hold queues that were gone before the
//     replicas arrived. Prefill's share of the scheduler queue is dropped
//     entirely: more prefill replicas do not fix a decode that is full.
package floor
