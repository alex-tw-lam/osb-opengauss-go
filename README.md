# osb-opengauss-go

An [Open Service Broker API](https://www.openservicebrokerapi.org/) broker in
Go that turns a shared **openGauss / GaussDB** instance into a self-service
catalog. This is the Go port of
[osb-opengauss](https://github.com/alex-tw-lam/osb-opengauss) (Python); both
implement the same SQL and the same behaviour.

| OSB concept | openGauss implementation |
|---|---|
| Service offering `gaussdb` | one openGauss instance (admin connection via env vars) |
| Service plan | a quota bundle: `PERM/TEMP/SPILL SPACE` + database `CONNECTION LIMIT` |
| Service instance (tenant) | a **logical database** owned by one NOLOGIN group role; `public` schema is the shared namespace |
| Binding | a **read-write login user** (`CREATE USER`) in the tenant's group; per-binding `ALTER DEFAULT PRIVILEGES` makes everything each binding creates visible to all others |

Built with [brokerapi](https://code.cloudfoundry.org/brokerapi/v13) (the
Cloud Foundry OSB library), [gaussdb-go](https://github.com/HuaweiCloudDeveloper/gaussdb-go)
and [GORM](https://gorm.io) (state storage). Requires Go **1.25.10** (pinned
in go.mod). The SQL statements this broker issues were validated against a live
openGauss 7.0.0-RC3 server and cross-checked against the GaussDB
(centralized V2.0-10.x) SQL reference; see the Python repository's
`docs/opengauss-research.md` for the full findings.

## Driver and authentication

The broker uses [gaussdb-go](https://github.com/HuaweiCloudDeveloper/gaussdb-go),
a pgx fork maintained by the same Huawei org as the Python repository's
driver. It speaks openGauss's native **sha256** authentication, so the secure
server default works out of the box:

* `password_encryption_type = 2` (sha256 only) — supported directly; no
  server-side workaround needed for the broker's admin connection.
* `password_encryption_type = 0`/`1` (md5) — also supported.
* Tenants take note: binding users are hashed with the server's current
  `password_encryption_type`, so applications connecting with ordinary
  PostgreSQL clients need the dual-hash setting (`1`) unless they too use a
  GaussDB-aware driver (verified live: gaussdb-go completes the sha256
  handshake against a type-2-only server).

## Plans

Plans live in [`plans.toml`](plans.toml) — deployment **data**, not code.
Each environment carries its own copy; the broker validates it at startup
and refuses to start on a missing, malformed or duplicate-id file. Fields:
`id`/`name`/`description` (never rename an id once instances exist on it),
`storage_gb` (PERM SPACE / tablespace MAXSIZE), `temp_gb`/`spill_gb`
(TEMP/SPILL SPACE), `max_connections` (database CONNECTION LIMIT) and the
optional `free` (defaults true).

Optional provision parameters (validated against the plan): `compatibility`
(`PG`/`A`/`B`/`C`), `encoding` (`UTF8`/`GBK`/`GB18030`/`Latin1`),
`tablespace` (enum of operator-curated tablespaces), `max_connections`,
`storage_gb`, `temp_gb`, `spill_gb`. Bind parameter: `max_connections`.

## State storage

The broker remembers its instances and bindings in two SQL tables through
GORM. The default backend is a SQLite file (`STATE_DB_PATH`, pure-Go driver,
no cgo). Setting `STATE_DSN` to a `postgres://` URL moves the state to any
PostgreSQL-compatible server; a `gaussdb://` URL moves it to openGauss
itself — including the instance the broker manages, using the same
sha256-capable driver (the tables then live in the admin user's schema).
Written portably: no upserts (`INSERT ... ON CONFLICT` is PostgreSQL 9.5+
and openGauss is 9.2 based), so record writes are explicit read-then-write.

## Storage sizing modes

`GAUSSDB_STORAGE_MODE` selects how the storage quota is enforced:

* **`role_quota` (default)** — `PERM SPACE` (+`TEMP SPACE`/`SPILL SPACE`) on
  the owner role and every binding user. Needs workload management enabled
  on the server for enforcement.
* **`tablespace`** — every instance gets a dedicated tablespace
  (`RELATIVE LOCATION 'broker/<ts>' MAXSIZE '<plan>G'`) set as the database
  default: a hard per-node storage cap. The admin user must be sysadmin and
  `storage_gb` updates map to `ALTER TABLESPACE ... RESIZE MAXSIZE`.

## Quick start

```bash
go build -o osb-opengauss .

export GAUSSDB_HOST=... GAUSSDB_PORT=5432
export GAUSSDB_ADMIN_USER=... GAUSSDB_ADMIN_PASSWORD=...
export BROKER_PASSWORD=$(openssl rand -hex 16)

./osb-opengauss        # listens on 127.0.0.1:5000 by default
```

`GET /healthz` (no authentication) runs `SELECT 1` over the same connection
path and answers `200 {"status":"ok"}` or `503` with the driver error — for
Kubernetes probes and load balancers.

## Trying it with curl

```bash
AUTH='-u broker:<password>'; H='X-Broker-API-Version: 2.16'
SID=4c6f6a1e-0f5a-4a5b-9d7e-2f8b3a1c5e01

curl $AUTH -H "$H" localhost:5000/v2/catalog

curl $AUTH -H "$H" -X PUT "localhost:5000/v2/service_instances/<uuid>?accepts_incomplete=false" \
  -H 'Content-Type: application/json' \
  -d "{\"service_id\":\"$SID\",\"plan_id\":\"gaussdb-dev\"}"

curl $AUTH -H "$H" -X PUT "localhost:5000/v2/service_instances/<uuid>/service_bindings/<uuid2>" \
  -H 'Content-Type: application/json' \
  -d "{\"service_id\":\"$SID\",\"plan_id\":\"gaussdb-dev\",\"parameters\":{\"access_role\":\"readonly\"}}"
# -> credentials: uri / hostname / port / database / username / password / jdbcUrl
```

## Configuration

Configuration comes exclusively from environment variables.

| Env var | Default | Meaning |
|---|---|---|
| `GAUSSDB_HOST` / `GAUSSDB_PORT` | `localhost` / `5432` | openGauss admin endpoint |
| `GAUSSDB_ADMIN_USER` / `GAUSSDB_ADMIN_PASSWORD` | `gaussdb` / — | needs SYSADMIN (the public schema in a new database is owned by the cluster initial user, so only SYSADMIN can grant on it) |
| `GAUSSDB_ADMIN_DB` | `postgres` | database for DDL |
| `GAUSSDB_SSLMODE` | `disable` | libpq sslmode, propagated in binding URIs |
| `GAUSSDB_CONNECT_TIMEOUT` | `10` | connection timeout (seconds) |
| `BROKER_USERNAME` / `BROKER_PASSWORD` | `broker` / dev default | OSB basic auth |
| `STATE_DB_PATH` | `./osb-opengauss-state.db` | SQLite state file (default backend) |
| `STATE_DSN` | *(empty)* | move the state to a PostgreSQL-compatible server: a `postgres://` URL, or `gaussdb://` for openGauss with native sha256 |
| `GAUSSDB_NAME_PREFIX` | `gdb` | prefix for created databases/roles/users |
| `GAUSSDB_STORAGE_MODE` | `role_quota` | `role_quota` or `tablespace` |
| `GAUSSDB_TABLESPACES` | *(empty)* | curated tablespace enum |
| `GAUSSDB_PLANS_FILE` | `plans.toml` | plan catalog data file |
| `GAUSSDB_TABLESPACE_LOCATION_PREFIX` | `broker` | single path segment under `pg_location/` |
| `BROKER_HOST` / `BROKER_PORT` | `127.0.0.1` / `5000` | HTTP bind |

## Code layout — one responsibility per file

| File | Responsibility |
|---|---|
| `main.go` | Wiring: builds everything, exposes `/healthz`, runs the HTTP server |
| `broker.go` | OSB layer: maps brokerapi calls to admin + store calls, maps errors to HTTP statuses |
| `plans.go` | Loads `plans.toml` (data), validates it, assembles the catalog |
| `params.go` | Request rules: parameter validation and the matching JSON schemas |
| `gaussdb.go` | All openGauss DDL; knows SQL, not the OSB API |
| `driver.go` | The database driver: gaussdb-go connections behind the DB interface |
| `state.go` | Memory: GORM records of what the broker created (SQLite or PostgreSQL-compatible) |
| `config.go` | Environment variable names and their parsing |

## Checks

Run before every change; all commands are expected to pass with zero findings:

```bash
gofmt -l . && go vet ./...
go test -cover ./...
staticcheck ./...
govulncheck ./...
gosec ./...        # one documented #nosec: the plans file path (operator config)
gitleaks detect --source . --no-git
grep -rnP '[\x{4E00}-\x{9FFF}]' --exclude-dir=.git .   # must print nothing
```
