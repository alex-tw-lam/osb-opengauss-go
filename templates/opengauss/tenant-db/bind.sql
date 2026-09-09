-- bind.sql: sets up per-binding default privileges so everything the user
-- creates in public is visible to the whole group.
-- Executed on the tenant database.

ALTER DEFAULT PRIVILEGES FOR ROLE {{.Username}}
  IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO {{.GroupRole}};

ALTER DEFAULT PRIVILEGES FOR ROLE {{.Username}}
  IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO {{.GroupRole}};
