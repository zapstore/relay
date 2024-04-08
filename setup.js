import { Database } from "bun:sqlite";
const db = new Database("relay.sqlite", { create: true });

db.exec("PRAGMA journal_mode = WAL;");

db.query(`CREATE TABLE IF NOT EXISTS events
   (id TEXT PRIMARY KEY,
    pubkey TEXT NOT NULL,
    sig TEXT NOT NULL,
    kind INTEGER NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    content TEXT,
    tags TEXT
    );`).run();

// TODO create indices for fields AND single-letter tags
// db.query(`CREATE INDEX id_idx ON events (id);`).run();

db.query(`CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(text, content='', tokenize=trigram, contentless_delete=1);`).run();

db.query(`CREATE TRIGGER if not exists events_ai AFTER INSERT ON events BEGIN
  INSERT INTO events_fts (rowid, text)
    SELECT new.rowid, new.content || ' ' || GROUP_CONCAT(json_extract(value, '$[1]'), ' ') as text
      FROM json_each(new.tags)
      WHERE json_extract(value, '$[0]') IN ('url', 'title', 'name', 'alt', 't');
END;`).run();

db.query(`CREATE TRIGGER if not exists events_ad AFTER DELETE ON events BEGIN
  DELETE FROM events_fts WHERE rowid = old.rowid;
END;`).run();