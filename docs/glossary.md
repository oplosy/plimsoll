# Plimsoll Domain Glossary

Terms used across backend and frontend, kept in one place.

- **Cargo** — the goods a shipment moves; the core resource of the domain.
- **Vessel** — the craft that carries cargo; identified by immutable registry data.
- **Route** — an ordered sequence of stops a shipment passes through.
- **Leg** — one hop between two stops on a route.
- **Manifest** — the declared list of cargo on a vessel for a given leg.

Conventions:

- Use domain nouns verbatim in API fields and UI copy.
- Avoid abbreviations in new code; existing ones stay until touched.
