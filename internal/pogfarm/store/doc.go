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
// README there for how to add one. The first migration creates the core
// tables (streamers, vods, candidates, jobs, clips, uploads) and documents
// the conventions of the schema. Each table has a row type here with plain
// Insert, Get and Update functions, and its status values are typed
// constants.
//
// Times are stored as text in UTC in one fixed-width format (TimeLayout), so
// they compare correctly as strings in SQL. Hand-written queries pass times
// through FormatTime.
package store
