# ADR: Lifetime Co-Activation Count per Association Pair

**Date:** 2026-03-04
**Status:** Implemented

## Context

MuninnDB's Hebbian engine tracked per-batch co-activation count transiently
in `pairStats.count` but discarded it after each consolidation pass. This lost
a fundamental cognitive signal: how many times have two engrams fired together
over their lifetime?

Without a persistent count, MuninnDB cannot distinguish:
- An association that fired once at high weight (episodic trace)
- An association that fired 500 times at moderate weight (habitual/semantic)

These represent very different memory types, with different durability
and reactivation expectations.

## Decision

Extend the association Pebble value from 22→26 bytes by appending a `uint32`
co-activation count at bytes 22-25. Thread the count through all write paths
(WriteAssociation, UpdateAssocWeight, UpdateAssocWeightBatch) and expose it
in the REST API via `GET /engrams/{id}/links`.

## Value Layout (26 bytes)

| Offset | Size | Field              | Encoding              |
|--------|------|--------------------|-----------------------|
| 0      | 2    | relType            | uint16 big-endian     |
| 2      | 4    | confidence         | float32 big-endian    |
| 6      | 8    | createdAt          | int64 UnixNano BE     |
| 14     | 4    | lastActivated      | uint32 Unix seconds BE|
| 18     | 4    | peakWeight         | float32 big-endian    |
| 22     | 4    | coActivationCount  | uint32 big-endian     |

## Backward Compatibility

- Old 22-byte values decode with `coActivationCount=0` ("pre-feature/unknown")
- Old binaries reading new 26-byte values ignore bytes 22-25 safely (progressive-length decoder)
- New associations created after this release start with `count=1` (creation is itself a co-activation)
- `count=0` means "unknown/pre-feature", not "never fired" — downstream logic must handle this distinction
- Count saturates at `math.MaxUint32` to prevent overflow

## What This Enables

- **Archiving decisions**: high count = preserve even at low weight (episodic vs semantic distinction)
- **Recall ranking**: count signals relationship stability independent of current weight
- **DeCue long-dormancy**: distinguish "decayed after one firing" from "decayed despite 500 firings"
- **API consumers**: `co_activation_count` available in `GET /engrams/{id}/links` with no additional work

## What Was Deferred

- **erf export format**: The erf package has its own 40-byte AssocRecordSize with exact-length checks. Adding count to erf requires a version bump and is a separate concern — left for a follow-up PR.
- **Structured record format (flatbuffers/protobuf)**: The progressive-length decoder pattern handles this extension cleanly. Trigger to revisit: when value exceeds ~30 bytes OR when optional/conditional fields are needed.
- **Archiving (0x25 prefix)**: Requires count as a prerequisite (now done) plus operational experience with the count signal before designing archiving policy.
- **Phase 1 replay composite scoring**: Deferred — Phase 1 replay is currently a stub and scoring improvements are premature until replay is wired.
