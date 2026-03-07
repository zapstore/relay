---
description: Quality expectations — when to spec, testing, anti-patterns, AI workflow
alwaysApply: true
---

# relay — Quality Bar

## When to Create a Feature Spec

Create a spec if the work:

- Changes event validation or kind allowlist behavior
- Modifies ACL logic or unknown pubkey policy
- Adds or changes rate limiting parameters
- Touches Blossom upload/download flow
- Changes analytics collection
- Modifies NIP-42 or NIP-98 auth handling

**Skip the spec** if:

- Config env var rename with backward compat
- Log message changes
- Dependency update with no API changes
- Bug fix with obvious cause and fix

## Testing

- ACL logic must be unit-tested with CSV fixtures.
- Rate limiter must be tested with time-controlled clocks.
- Event validation must be tested against malformed and valid event fixtures.
- No real network calls in tests — mock Bunny CDN and Vertex DVM.

## Implementation Expectations

- Keep pkg boundaries clean — relay and blossom must not import each other.
- Analytics writes must always be non-blocking (channel send with select/default).
- Hot-reload logic must be tested for both valid and invalid CSV inputs.

## Anti-Patterns

- Blocking request handlers on slow external calls (Vertex DVM, Bunny CDN)
- Clearing ACL state on parse error
- Storing blob content locally (CDN redirect only)
- Accepting events without signature verification

## Working With AI

- Spec-first for auth, ACL, and event validation changes.
- Work packets in `spec/work/` for non-trivial tasks.
- Never modify `spec/guidelines/` without explicit permission.
- If behavior conflicts with NIP specs, stop and report — do not guess.

## Knowledge Entries

After a work packet merges, promote non-obvious decisions to `spec/knowledge/DEC-XXX-*.md`. See `spec/knowledge/_TEMPLATE.md` for format and criteria.
