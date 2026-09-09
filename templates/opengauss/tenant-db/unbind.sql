-- unbind.sql: terminates the user's sessions and drops everything it owns.
-- Executed on the tenant database.

SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = {{.UsernameLiteral}};

DROP OWNED BY {{.Username}} CASCADE;
