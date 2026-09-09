-- bind.sql: creates a read-write login user and joins it to the group role.
-- Executed on the admin database.

CREATE USER {{.Username}} LOGIN PASSWORD {{.Password}};

GRANT {{.GroupRole}} TO {{.Username}};
