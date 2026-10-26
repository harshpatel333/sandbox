Exact read-only fixtures from sandbox PR 761, head
40275310857f163361947df1ee858bb8326578f1. They reproduce the existing staging
provider-pair schema in an isolated test database. These files are never part of
the production migration directory. Production must not import these features
just to apply the launch migrations.
