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

## Service details

Each `<type>.one` procedure takes the service ID as a query parameter named
after the type (`applicationId`, `composeId`, `postgresId`, `mysqlId`,
`mariadbId`, `mongoId`, `redisId`, `libsqlId`) and returns the table row plus
the relations loaded by `find<Type>ById` in `packages/server/src/services/`.
Only a representative subset of columns is kept; secrets are placeholders.

| Fixture | Router (`apps/dokploy/server/api/routers/`) | Columns (`packages/server/src/db/schema/`) | Relations (`packages/server/src/services/`) |
| --- | --- | --- | --- |
| `application.one.json` | `application.ts` `one` (adds `hasGitProviderAccess`, `unauthorizedProvider`) | `application.ts`; `ports` rows from `port.ts`; `domains` rows from `domain.ts` | `application.ts` `findApplicationById` |
| `compose.one.json` | `compose.ts` `one` | `compose.ts` (`composeType`, `isolatedDeployment`, `serviceNetworks`); `domain.ts` | `compose.ts` `findComposeById` |
| `postgres.one.json` | `postgres.ts` `one` | `postgres.ts` (`networkIds`, `detachDokployNetwork`) | `postgres.ts` `findPostgresById` |
| `mysql.one.json` | `mysql.ts` `one` | `mysql.ts` | `mysql.ts` `findMySqlById` |
| `mariadb.one.json` | `mariadb.ts` `one` | `mariadb.ts` | `mariadb.ts` `findMariadbById` |
| `mongo.one.json` | `mongo.ts` `one` | `mongo.ts` (`networkSwarm` override) | `mongo.ts` `findMongoById` |
| `redis.one.json` | `redis.ts` `one` | `redis.ts` (remote `serverId`, `server`) | `redis.ts` `findRedisById` |
| `libsql.one.json` | `libsql.ts` `one` | `libsql.ts` (`externalGRPCPort`, `externalAdminPort`) | `libsql.ts` `findLibsqlById` |

The fixed database ports the client reports (postgres 5432, mysql and
mariadb 3306, mongo 27017, redis 6379, libsql 8080 HTTP, 5001 gRPC and 5000
admin) are the `TargetPort` values in
`packages/server/src/utils/databases/<type>.ts`.

## Compose services

`compose.loadServices` takes `composeId` and `type` as query parameters
(`apiFetchServices` in `packages/server/src/db/schema/compose.ts`: `type` is
`"fetch"` or `"cache"`, defaulting to `"cache"`). doktunnel always sends
`type=cache`, which reads the compose file already on the server; `fetch`
would `git clone` the compose source first. The router
(`apps/dokploy/server/api/routers/compose.ts`, `loadServices`) checks
`service: read` and the member's access to that compose service
(`checkServicePermissionAndAccess`), and `loadServices` in
`packages/server/src/services/compose.ts` returns the keys of the file's
`services` map, or throws `NOT_FOUND` ("Services not found") when no file is
cached.

| Fixture | Procedure | Mirrors |
| --- | --- | --- |
| `compose.loadServices.json` | `compose.loadServices?composeId=cmp_stack&type=cache` | the service names of the `composeFile` in `compose.one.json` |
