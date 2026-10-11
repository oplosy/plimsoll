# Local development commands

The root Makefile is the command entry point. Run make help to see every target and its current description.

## Checks

- make test runs Go unit tests for pure engines and does not need Docker.
- make test-integration runs the integration-tagged Go suite against the local Compose services.
- make lint runs golangci-lint.
- make frontend-check runs dashboard type checks, the no-float-parsing guard, and money tests.
- make docs-check verifies that AGENTS.md and CLAUDE.md are byte-identical.

## Local stack and database

- make up rebuilds the application image, applies embedded migrations, starts the stack, and waits for health.
- make migrate applies migrations with PLIMSOLL_OWNER_DSN; make migrate-down reverts one step using the same owner role.
- make psql opens a session using PLIMSOLL_APP_DSN, so its permissions reflect the application role.
- make smoke runs the end-to-end check through Caddy.

make down runs docker compose down -v, which removes the local Compose volumes as well as stopping the services. Do not use it when you need to keep the local database state.