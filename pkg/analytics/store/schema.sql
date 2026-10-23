CREATE TABLE IF NOT EXISTS relay_metrics (
  day           DATE NOT NULL,
  reqs          INTEGER NOT NULL DEFAULT 0, -- REQs fulfilled
  filters       INTEGER NOT NULL DEFAULT 0, -- filters fulfilled
  events        INTEGER NOT NULL DEFAULT 0, -- events saved or replaced
  PRIMARY KEY (day)
);

CREATE TABLE IF NOT EXISTS bundle_requests (
  day            DATE NOT NULL,
  from_epoch     INTEGER NOT NULL, -- -1 when from was not a valid epoch
  to_epoch       INTEGER NOT NULL, -- -1 when the latest snapshot was not read
  status         INTEGER NOT NULL,
  protocol       TEXT NOT NULL, -- path version, such as v1; empty when the path had none
  client_version TEXT NOT NULL, -- query v; empty when absent or not a version label
  count          INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, from_epoch, to_epoch, status, protocol, client_version)
);

CREATE INDEX IF NOT EXISTS bundle_requests_day ON bundle_requests (day);
CREATE INDEX IF NOT EXISTS bundle_requests_status ON bundle_requests (status);
CREATE INDEX IF NOT EXISTS bundle_requests_protocol ON bundle_requests (protocol);
CREATE INDEX IF NOT EXISTS bundle_requests_client_version ON bundle_requests (client_version);
