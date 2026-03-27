# Architecture Rules TBD

This placeholder exists so agents can reference a stable location without inventing repository-wide architecture law.

## To be filled by repo owner
- canonical service boundaries
- cross-service communication contracts
- ownership of launcher UI versus daemon logic
- persistence and state ownership rules
- migration or compatibility policy

## Current interim guidance
- Prefer small services with explicit responsibilities.
- Avoid spreading business logic across transport or glue layers.
- Document trade-offs in code comments when architecture guidance is missing.
