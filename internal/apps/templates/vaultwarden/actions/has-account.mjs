#!/usr/bin/env node
// Counts Vaultwarden accounts. The volume is mounted read-only, so the database (and its
// write-ahead log) is copied first; SQLite cannot open a WAL database on a read-only disk.
import { copyFileSync, existsSync } from "node:fs";
import { DatabaseSync } from "node:sqlite";

const src = "/volumes/data/db.sqlite3";
if (!existsSync(src)) {
  console.log(JSON.stringify({ done: false, accounts: 0 }));
  process.exit(0);
}
for (const suffix of ["", "-wal", "-shm"]) {
  if (existsSync(src + suffix)) copyFileSync(src + suffix, "/tmp/db.sqlite3" + suffix);
}
const db = new DatabaseSync("/tmp/db.sqlite3");
const { n } = db.prepare("select count(*) as n from users").get();
console.log(JSON.stringify({ done: n > 0, accounts: n }));
