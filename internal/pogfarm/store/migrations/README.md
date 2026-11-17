# Migrations

Each file in this directory is one schema migration, embedded into the pogfarm
binary and applied by `store.Open`.

- Name: `NNNN_short_name.sql`, for example `0001_core_tables.sql`. The number
  is the version; the name is lower-case letters, digits and underscores.
- Versions are unique and only ever grow. Take the next free number.
- A file may hold several statements. The whole file runs in one transaction,
  so do not write `BEGIN` or `COMMIT`, and a failing statement leaves the
  database as it was before the file.
- Never edit or delete a migration that has been merged: databases that
  already applied it will not run it again. Add a new file instead.
- `PRAGMA foreign_keys` cannot be changed inside a transaction, so it stays on
  while a migration runs.

Files that do not end in `.sql` are ignored. This file also keeps the
directory present before the first migration exists.
