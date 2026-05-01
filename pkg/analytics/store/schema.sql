CREATE TABLE IF NOT EXISTS app_impressions (
  app_id        TEXT NOT NULL,
  app_pubkey    TEXT NOT NULL,
  app_version   TEXT NOT NULL DEFAULT '',
  day           DATE NOT NULL,
  source        TEXT NOT NULL,
  type          TEXT NOT NULL,
  country_code  TEXT,
  count         INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (app_id, app_pubkey, app_version, day, source, type, country_code)
);

CREATE INDEX IF NOT EXISTS app_impressions_app_pubkey ON app_impressions (app_pubkey);
CREATE INDEX IF NOT EXISTS app_impressions_app_version ON app_impressions (app_version);
CREATE INDEX IF NOT EXISTS app_impressions_day ON app_impressions (day);
CREATE INDEX IF NOT EXISTS app_impressions_source ON app_impressions (source);
CREATE INDEX IF NOT EXISTS app_impressions_type ON app_impressions (type);
CREATE INDEX IF NOT EXISTS app_impressions_country_code ON app_impressions (country_code);

CREATE TABLE IF NOT EXISTS app_downloads (
  hash          TEXT NOT NULL,
  app_id        TEXT NOT NULL DEFAULT '',
  app_version   TEXT NOT NULL DEFAULT '',
  app_pubkey    TEXT NOT NULL DEFAULT '',
  day           DATE NOT NULL,
  source        TEXT NOT NULL,
  type          TEXT NOT NULL,
  country_code  TEXT,
  count         INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (hash, day, source, type, country_code)
);

CREATE INDEX IF NOT EXISTS app_downloads_app_id ON app_downloads (app_id);
CREATE INDEX IF NOT EXISTS app_downloads_app_pubkey ON app_downloads (app_pubkey);
CREATE INDEX IF NOT EXISTS app_downloads_day ON app_downloads (day);
CREATE INDEX IF NOT EXISTS app_downloads_source ON app_downloads (source);
CREATE INDEX IF NOT EXISTS app_downloads_type ON app_downloads (type);
CREATE INDEX IF NOT EXISTS app_downloads_country_code ON app_downloads (country_code);


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
