-- provision.sql: creates the tenant's group role, tablespace and database.
-- Executed on the admin database (usually postgres).

CREATE ROLE {{.GroupRole}} NOLOGIN PASSWORD {{.GroupPassword}};

-- Temporary membership: CREATE DATABASE OWNER and CREATE TABLESPACE OWNER
-- require the executing user to be a member of the owner role.
GRANT {{.GroupRole}} TO {{.AdminUser}};

CREATE TABLESPACE {{.Tablespace}} OWNER {{.GroupRole}}
  RELATIVE LOCATION {{.TablePrefix}} MAXSIZE {{.StorageQuota}};

CREATE DATABASE {{.Database}} OWNER {{.GroupRole}}
  TEMPLATE template0
  ENCODING {{.Encoding}}
  DBCOMPATIBILITY {{.Compatibility}}
  TABLESPACE {{.Tablespace}}
  CONNECTION LIMIT {{.MaxConnections}};

-- Lock connection isolation: only the group role and the broker admin
-- may connect to this database.
REVOKE CONNECT ON DATABASE {{.Database}} FROM PUBLIC;
GRANT CONNECT ON DATABASE {{.Database}} TO {{.GroupRole}};
GRANT CONNECT ON DATABASE {{.Database}} TO {{.AdminUser}};

-- Drop the temporary membership used for CREATE DATABASE OWNER.
REVOKE {{.GroupRole}} FROM {{.AdminUser}};
