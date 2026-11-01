# API Docs Index

Where API documentation lives for plimsoll and how to keep it findable.

## Sources

- Backend route definitions — the canonical map of endpoints.
- `docs/` — conceptual docs that explain flow and constraints above the endpoint level.
- `deploy/` — environment-specific notes on what is reachable and how.

## Rules

- Every new endpoint gets a line next to its route definition before merge.
- Conceptual docs link to code; code does not link back to docs (docs may rot faster).
- Deleting an endpoint removes both its code and its docs in the same change.

This index may lag; when it does, fix the doc, not the habit of checking code first.
