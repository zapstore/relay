CREATE TABLE IF NOT EXISTS impressions (
  app_id        TEXT NOT NULL,
  app_pubkey    TEXT NOT NULL,
  day           DATE NOT NULL,
  source        TEXT NOT NULL,
  type          TEXT NOT NULL,
  country_code  TEXT,
  count         INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (app_id, app_pubkey, day, source, type, country_code)
);

CREATE INDEX IF NOT EXISTS impressions_app_pubkey ON impressions (app_pubkey);
CREATE INDEX IF NOT EXISTS impressions_day ON impressions (day);
CREATE INDEX IF NOT EXISTS impressions_source ON impressions (source);
CREATE INDEX IF NOT EXISTS impressions_type ON impressions (type);
CREATE INDEX IF NOT EXISTS impressions_country_code ON impressions (country_code);

CREATE TABLE IF NOT EXISTS downloads (
  hash          TEXT NOT NULL,
  day           DATE NOT NULL,
  source        TEXT NOT NULL,
  type          TEXT NOT NULL,
  country_code  TEXT,
  count         INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (hash, day, source, type, country_code)
);

CREATE INDEX IF NOT EXISTS downloads_day ON downloads (day);
CREATE INDEX IF NOT EXISTS downloads_source ON downloads (source);
CREATE INDEX IF NOT EXISTS downloads_type ON downloads (type);
CREATE INDEX IF NOT EXISTS downloads_country_code ON downloads (country_code);


CREATE TABLE IF NOT EXISTS relay_metrics (
  day           DATE NOT NULL,
  reqs          INTEGER NOT NULL DEFAULT 0, -- REQs fulfilled
  filters       INTEGER NOT NULL DEFAULT 0, -- filters fulfilled
  events        INTEGER NOT NULL DEFAULT 0, -- events saved or replaced
  PRIMARY KEY (day)
);

CREATE TABLE IF NOT EXISTS blossom_metrics (
  day           DATE NOT NULL,
  checks        INTEGER NOT NULL DEFAULT 0, -- checks that succeeded
  downloads     INTEGER NOT NULL DEFAULT 0, -- downloads that succeeded
  uploads       INTEGER NOT NULL DEFAULT 0, -- uploads that hit bunny
  PRIMARY KEY (day)
);
