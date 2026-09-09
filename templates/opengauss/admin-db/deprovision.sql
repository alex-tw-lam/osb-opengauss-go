-- deprovision.sql: removes the whole tenant (database, tablespace, role).
-- Executed on the admin database.

-- DROP DATABASE requires membership in the owner role.
GRANT {{.GroupRole}} TO {{.AdminUser}};

SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = {{.DatabaseLiteral}};

DROP DATABASE IF EXISTS {{.Database}};

DROP TABLESPACE IF EXISTS {{.TableSpace}};

REVOKE {{.GroupRole}} FROM {{.AdminUser}};

DROP ROLE IF EXISTS {{.GroupRole}};
