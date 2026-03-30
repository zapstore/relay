# NIP-XX: REQ Resolve (Tag Dereference)

**Status:** Draft  
**Depends on:** NIP-01, NIP-11, NIP-50

---

## Summary

Adds a `resolve` extension to NIP-50 `search` filter fields that instructs a relay to also return events *referenced by* the matched events via specified tag types — in the same subscription, before `EOSE`.

This eliminates a round-trip for common patterns where a client fetches an event and immediately needs the events it references (e.g. a release event and its asset events).

---

## Wire Format

A client adds a second filter to the `REQ` containing only the resolve instructions as NIP-50 `key:value` extensions:

```json
["REQ", "sub1",
  {"kinds": [30063], "#d": ["com.example.app"]},
  {"search": "resolve:e resolve_event_limit:5 resolve_limit:100"}
]
```

### Extension keys

| Key | Value | Description |
|-----|-------|-------------|
| `resolve` | single-letter tag name | Tag type to dereference. Repeatable. |
| `resolve_event_limit` | integer | Max resolved events contributed per matched event. Optional. |
| `resolve_limit` | integer | Hard cap on total resolved events across the whole query. Optional. |

`resolve` may appear multiple times to follow more than one tag type:

```
resolve:e resolve:a resolve_event_limit:5 resolve_limit:200
```

Supported resolve targets:

| Tag | Resolves to |
|-----|-------------|
| `e` | Event by ID |
| `a` | Replaceable/addressable event by `kind:pubkey:d-tag` |
| `p` | Kind-0 metadata for the referenced pubkey |

### Limits

- `resolve_event_limit` — applied per matched event, after deduplicating tag values. Protects against fat events with many references.
- `resolve_limit` — applied to the total resolved result set, after deduplication across all matched events. Protects against wide queries.
- Both are optional. Omitting either means the relay applies its own defaults (see NIP-11 below).
- Client values above the relay's advertised maximums are silently clamped.

### Depth

Always **1**. Resolved events are not themselves resolved recursively.

---

## Relay Behavior

1. Execute the primary filters normally.
2. For each matched event, collect tag values for each requested `resolve` tag type.
3. Deduplicate collected IDs/coordinates across all matched events.
4. Apply `resolve_event_limit` per source event, then `resolve_limit` globally.
5. Fetch referenced events from local storage.
6. Send primary events and resolved events in the same subscription stream, before `EOSE`.
7. If a referenced event is not in local storage, skip it silently — no error.
8. Resolved events MUST pass the same ACL and kind allowlist checks as primary events.

---

## NIP-11 Advertisement

Relays signal support by including the NIP number in `supported_nips`. Hard caps on resolve limits are advertised in `limitation`:

```json
{
  "supported_nips": [1, 11, 50, 42, "XX"],
  "limitation": {
    "resolve_max_event_limit": 10,
    "resolve_max_limit": 500
  }
}
```

`resolve_max_event_limit` and `resolve_max_limit` are the relay's hard ceilings. Clients SHOULD read these before constructing resolve filters.

---

## Client Behavior

Clients MUST check NIP-11 before sending resolve filters. If the relay does not advertise support, clients fall back to a second REQ:

```
fetch NIP-11
if "XX" in supported_nips:
    send REQ with primary filter + resolve filter
    cap resolve_limit to limitation.resolve_max_limit
else:
    send REQ with primary filter only
    on EOSE, collect e-tag IDs from result set
    send second REQ: {"ids": [<collected ids>]}
```

Clients SHOULD NOT send resolve filters to relays that do not advertise NIP-XX support, as NIP-50 relays may attempt to text-search the extension string and return unrelated events.

---

## Backwards Compatibility

- Relays without NIP-50 support: ignore the `search` field entirely; the second filter matches nothing. The primary filter works normally.
- Relays with NIP-50 but without NIP-XX: may attempt to text-search the extension string. Clients that check NIP-11 first never reach this case.
- The `key:value` extension syntax used here is explicitly defined in NIP-50: *"relays SHOULD ignore extensions they don't support."*

---

## Example: Zapstore Release + Assets

Without resolve (2 round trips):

```json
["REQ", "sub1", {"kinds": [30063], "#d": ["com.example.app"]}]
// → EOSE, client collects e-tag IDs from result
["REQ", "sub2", {"ids": ["<asset-id-1>", "<asset-id-2>", ...]}]
// → EOSE
```

With resolve (1 round trip):

```json
["REQ", "sub1",
  {"kinds": [30063], "#d": ["com.example.app"]},
  {"search": "resolve:e resolve_limit:20"}
]
// → release event + asset events + EOSE
```

---

## Open Questions

- Should resolved events be tagged with a marker (e.g. a synthetic `RESOLVED` message type) so clients can distinguish them from primary results? Current spec: no — clients track by event ID.
- Should `p` resolution be gated separately in NIP-11 (kind-0 fetches may be more expensive)?
- Should the resolve filter be allowed to carry additional filter conditions (e.g. `kinds` restriction on resolved events)?
