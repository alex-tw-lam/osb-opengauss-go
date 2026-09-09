-- unbind.sql: removes the user's auto-created schema and the user itself.
-- Executed on the admin database.

-- openGauss auto-creates a same-named schema for new users in the
-- database where they are created; clean it up.
DROP SCHEMA IF EXISTS {{.Username}} CASCADE;

DROP USER IF EXISTS {{.Username}};
