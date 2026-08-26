---
description: Bulk block retrieval endpoint — wire protocol and client implementer notes
---

# Bulk Block Retrieval (`/aurora/bulk/v0`)

Curio serves batched block retrievals: a client posts an ordered list of
block cids and receives a stream of blocks, served from merged sequential
piece reads instead of one random read per block. It exists for gateways
that already know their read plan — the ordered leaf list of an object and
(optionally) which piece each leaf lives in — and turns that plan into
sector-sequential disk I/O on the SP.

This document is the normative wire spec plus implementation notes for
client authors. Reference implementations:

- server: `lib/curetr/bulk.go` (scheduler/handler), `lib/curetr/bulkproto.go` (wire types)
- Go client: `retlib/bulkproto` + `cmd/ipfsbench` (`bulk` mode) in the
  `curetr` benchmarking repo; the frame reader is ~40 lines

The original design rationale (parallelism model, fallback ladder, why
cid-addressed rather than piece-range) is in the gateway-side proposal
document (`doc/curio-bulk-retrieval.md` in the filecoin-gateway repo).

## Endpoints

```
POST /aurora/bulk/v0/blocks    request: dag-cbor, response: frame stream
GET  /aurora/bulk/v0/info      response: JSON capability document
```

No authentication — same public exposure as `/ipfs`. Requests over the
server's concurrency limit get HTTP 429; malformed or over-limit requests
get HTTP 400 with a text body and no frames.

## Request

Body is a dag-cbor map (unknown keys ignored, absent keys are zero):

| key      | type        | meaning |
|----------|-------------|---------|
| `blocks` | `[CID...]`  | requested blocks, in the client's intended consumption order; 1..maxBlocks |
| `window` | `uint`      | max out-of-order distance of response frames; 0 = server default; ≤ maxWindow |
| `pieces` | `[CID...]`  | optional piece cid hints, parallel to `blocks` (same length or absent) |

CIDs use standard dag-cbor encoding (tag 42, byte string with a `0x00`
multibase-identity prefix) — what any cbor-gen / dag-cbor library
produces. `Content-Type` on the request is not checked; senders should use
`application/cbor`.

Ordering matters: consecutive `blocks` entries that are physically
adjacent in a piece are what the server merges into sequential reads. Send
consumption order; for data written as contiguous runs that is also disk
order.

Piece hints skip cid→piece resolution server-side (one index query per
block instead of two, none when the piece's block index is cached in
memory). A wrong hint fails only that block (status 1); the server may
also serve a hinted block from a *different* piece than hinted when its
caches know a better copy — blocks are content-addressed, so the bytes are
the same.

## Response

`200 OK`, `Content-Type: application/vnd.aurora.bulk.v0`, body is a
stream of frames:

```
uvarint idx      index into request.blocks
uvarint status   0 = ok, 1 = not found, 2 = error
uvarint len      payload length (0 unless ok)
len bytes        block payload (raw block bytes, no CAR framing)
```

`uvarint` is unsigned LEB128 (Go `encoding/binary`, multiformats-varint
compatible). The stream is terminated by an **end frame**:
`idx = len(blocks), status = 0, len = 0`.

Contract:

- Exactly one frame per requested index, in any order, followed by the
  end frame. Duplicate or out-of-range indices are a server bug; clients
  should drop the stream if they see one.
- **Window rule**: a frame with index `i` is only emitted while
  `i < lowest_unemitted_index + window`. A client therefore never needs
  to buffer more than `window` frames to restore request order. (The
  server currently schedules in consecutive window-sized batches, which
  is strictly tighter than the rule; do not rely on batch boundaries.)
- `status 1` (not found): the block is not indexed on this provider —
  retry elsewhere, not here.
- `status 2` (error): indexed but not servable — no unsealed copy, read
  or verification failure, or denylisted. Per-block fallback; the rest of
  the stream is unaffected.
- Identity-multihash cids are answered inline from the cid digest.
- A connection that ends without the end frame is a failed stream; since
  requests are stateless, resume = re-request the unserved suffix.

**Clients MUST hash-verify every ok frame** against the requested cid.
The server verifies what it reads, but the transport and any middleboxes
are untrusted, and verification is what makes per-frame data self-
certifying.

## Capability discovery

`GET /aurora/bulk/v0/info` returns JSON:

```json
{"version":1,"addressing":["cid"],"maxBlocks":4096,"maxWindow":256,"maxStreams":8}
```

- `version`/`addressing` gate future variants (piece-range addressing
  would appear as `"piece-range"`).
- `maxBlocks`, `maxWindow`: hard per-request limits — exceeding either is
  a 400.
- `maxStreams`: advisory per-client concurrent stream count; the server
  cannot attribute streams to clients and enforces only a global
  concurrency cap (429 when exhausted).

Values reflect live server configuration and can change; cache the
document (the gateway uses ~1h positive / ~10min negative) rather than
probing per request. A 404 on `/info` means no bulk support — fall back
to block-granular retrieval.

## Client implementation checklist

1. Probe `/info` per endpoint, cache positive ~1h / negative ~10min.
2. Segment the read plan into per-run requests: consecutive blocks that
   live in the same piece, up to `maxBlocks` per request.
3. Run K concurrent streams (K ≤ `maxStreams`, one per run) against
   different sectors; per-stream TCP backpressure is the flow control.
4. Reorder with a `window`-frame buffer; deliver in order.
5. Verify every ok frame's hash. Treat mismatch as a failed stream and
   demote the endpoint.
6. status 1/2 → per-block fallback to `/ipfs/<cid>?format=raw`; stream
   death → re-request the unserved suffix; repeated failures → negative
   capability cache.

## Server behavior and tuning

Per request the server: resolves cids to piece locations in parallel
(offset cache → index, hints skip piece resolution), buckets by piece,
sorts by offset, merges blocks closer than a configurable gap into ranges,
and reads each range with a single ranged request to the storage node —
one sequential disk span. One shared piece-reader handle is held per piece
for the whole request. Unsealed copies only; this endpoint never triggers
unsealing.

All limits live in the `HTTP.BulkRetrieval` config section and are
dynamic (changes apply to running nodes): `MaxConcurrentStreams`,
`MaxBlocks`, `MaxWindow`, `DefaultWindow`, `AdvertisedMaxStreams`,
`MergeGapKiB`, `MaxRangeMiB`, `ResolveParallelism`. Peak buffer memory is
bounded by `MaxConcurrentStreams × MaxRangeMiB`.

Observability: `curio_curetr_bulk_*` metrics — request/frame counters,
bytes served, request/resolve/read latencies, and the merge-efficiency
distributions `bulk_range_bytes` / `bulk_range_blocks` (healthy
sequential traffic shows many blocks and many MiB per range read).
