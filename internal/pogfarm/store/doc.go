// Package store owns pogfarm's SQLite file: it opens the database, applies
// the embedded migrations and hands out the handles the other pogfarm
// packages query through.
//
// Only the pogfarm process opens the file. SQLite allows one writer at a
// time, so all writes go through a single connection (Store.Writer and
// Store.WriteTx) where they queue instead of failing with "database is
// locked". Reads use a separate pool of read-only connections (Store.Reader)
// and, in WAL mode, run concurrently with each other and with the writer.
//
// The schema lives in migrations/, one numbered SQL file per change; see the
// README there for how to add one.
package store
