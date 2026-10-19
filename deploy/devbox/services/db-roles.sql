-- The image sets only supabase_admin's password; Auth and the data API log in
-- as these roles.
SET log_statement = 'none';
SET log_min_error_statement = 'panic';
SET pgaudit.log = 'none';
SET log_min_duration_statement = -1;
\set pgpass `echo "$POSTGRES_PASSWORD"`
ALTER USER authenticator WITH PASSWORD :'pgpass';
ALTER USER supabase_auth_admin WITH PASSWORD :'pgpass';
