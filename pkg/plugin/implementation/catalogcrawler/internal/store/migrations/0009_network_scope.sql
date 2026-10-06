-- Catalog ids are unique within ONE network, not across networks.
--
-- Two networks fronting the same upstream publish the same catalog id --
-- "catalog:weather-observation:mausamgram" is produced by this deployment's own
-- crawl AND by any other network serving Mausamgram. Keyed on catalog_id alone,
-- those are one row: an Enqueue coalesces them into each other and a Complete
-- overwrites the other's cursor. Silently, with no error anywhere.
--
-- So both are keyed on (network_id, catalog_id).
--
-- EMPTY STRING means this deployment's own crawl -- the local providers reached
-- through a catalog index. It is deliberately not the configured network id: a
-- migration cannot know it, and using it would orphan every existing row behind
-- a value that did not exist when they were written. Local stays '', and only
-- cross-network rows carry a real network id.

ALTER TABLE crawler_queue   ADD COLUMN IF NOT EXISTS network_id text NOT NULL DEFAULT '';
ALTER TABLE crawler_catalog ADD COLUMN IF NOT EXISTS network_id text NOT NULL DEFAULT '';

ALTER TABLE crawler_queue DROP CONSTRAINT IF EXISTS crawler_queue_catalog_id_key;
CREATE UNIQUE INDEX IF NOT EXISTS ux_crawler_queue_network_catalog
  ON crawler_queue (network_id, catalog_id);

ALTER TABLE crawler_catalog DROP CONSTRAINT IF EXISTS crawler_catalog_pkey;
ALTER TABLE crawler_catalog ADD PRIMARY KEY (network_id, catalog_id);

-- The content a cross-network crawl fetched, waiting to be published.
--
-- The index-sourced path needs no such table: SyncNext re-fetches the index at
-- claim time, so the content is always re-obtainable from its URL. A discover
-- answers with the catalogs themselves and there is no per-catalog URL to go
-- back to, so the document is held here until its queue row is worked.
--
-- This is also what bounds memory. A crawl writes each page here and forgets
-- it; the worker reads one row at a time.
CREATE TABLE IF NOT EXISTS crawler_staged_catalog (
  network_id  text        NOT NULL,
  catalog_id  text        NOT NULL,
  -- What this copy is addressed to, which is the audience the discover asked
  -- for. Carried because the publish needs it and the document does not have
  -- it: visibleTo is a publish directive and is never returned by a discover.
  audience    text        NOT NULL,
  document    jsonb       NOT NULL,
  fetched_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (network_id, catalog_id)
);
