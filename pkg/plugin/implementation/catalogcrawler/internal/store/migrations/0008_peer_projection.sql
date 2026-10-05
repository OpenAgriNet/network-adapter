-- peer_projection records what we are currently holding from each peer network.
--
-- WHY A TABLE AT ALL. Discovery-service has no delete and no "whose copy is
-- this" column, so once a crawled catalog is published there, nothing in the
-- system can name it again as a peer's. Removing it later -- when its licence
-- expires, when the peer drops it, when the peer is suspended -- means being
-- able to say which catalogs those are. This is the only record of that.
--
-- It holds NO catalog content. The document lives in discovery-service, where
-- it is served from; a second copy here would be a second thing to keep in
-- step, for no gain. An id and an expiry are enough to purge by.
CREATE TABLE IF NOT EXISTS peer_projection (
    -- The peer that published it, not us. Scoped this way so one peer's
    -- suspension or expiry never touches another's.
    network_id text        NOT NULL,
    catalog_id text        NOT NULL,

    -- When our licence to keep this lapses: the moment of the crawl plus the
    -- projectionTtl the peer declared. Stored as an absolute instant rather
    -- than a ttl, so a peer shortening its ttl takes effect on the next crawl
    -- without rewriting history, and so expiry is a plain comparison.
    expires_at timestamptz NOT NULL,

    -- Diagnostic: the last pass that actually saw this catalog. Not used to
    -- decide anything -- expires_at is -- but it is the first thing anyone asks
    -- when a projection vanishes.
    last_seen_at timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (network_id, catalog_id)
);

-- Expiry sweeps scan by time across all peers, so the index is on the instant.
CREATE INDEX IF NOT EXISTS peer_projection_expires_at_idx
    ON peer_projection (expires_at);
