-- update.sql: changes the connection limit and the storage cap.
-- Executed on the admin database.

-- ALTER DATABASE requires membership in the owner role.
GRANT {{.GroupRole}} TO {{.AdminUser}};

ALTER DATABASE {{.Database}} CONNECTION LIMIT = {{.MaxConnections}};

-- Resize the tenant's storage cap. If the new quota is below current
-- usage the change still succeeds, but writes are blocked until usage
-- drops under the new limit.
ALTER TABLESPACE {{.TableSpace}} RESIZE MAXSIZE {{.StorageQuota}};

REVOKE {{.GroupRole}} FROM {{.AdminUser}};
