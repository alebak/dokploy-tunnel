# Dokploy API fixtures

Recorded response shapes that the tests in this package serve through
`httptest`. They are not captured from a live panel: each one is built from
the Dokploy source that produces the response, at commit
[`48504fd`](https://github.com/Dokploy/dokploy/tree/48504fde4eb210056f7d9f80406f9692a1a7ea8a)
of `Dokploy/dokploy` (canary, 2026-10-01). IDs, names and secrets are made up.

Dokploy serves every tRPC query as `GET /api/<router>.<procedure>` with its
input as query parameters (see the published OpenAPI document,
`Dokploy/website` `apps/docs/public/openapi.json`), and returns the procedure
output as plain JSON.

| Fixture | Procedure | Mirrors |
| --- | --- | --- |
| `project.all.owner.json` | `project.all`, key of an owner or admin | `apps/dokploy/server/api/routers/project.ts` (`all`, second `findMany`): project columns, environments with `environmentId`, `name`, `isDefault`; applications and compose with ID, name and status; databases with their ID only; `projectTags` with `tag` |
| `project.all.member.json` | `project.all`, key of a member | same file, first `findMany`: every service type with ID, name and status, filtered by the member's grants |

Table columns come from `packages/server/src/db/schema/project.ts`,
`environment.ts` and the per-type schema files.
