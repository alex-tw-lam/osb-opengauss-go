-- provision.sql: sets up the tenant database's shared public schema.
-- Executed on the newly created tenant database.

-- Object isolation: ordinary users only see objects they may access.
ALTER DATABASE {{.Database}} ENABLE PRIVATE OBJECT;

-- The tenant's group role gets full access to the shared namespace.
GRANT USAGE, CREATE ON SCHEMA public TO {{.GroupRole}};

-- Allow the group role to create additional schemas (opt-in namespaces).
GRANT CREATE ON DATABASE {{.Database}} TO {{.GroupRole}};
