package store

import (
	"context"
	"time"
)

// Projection names one catalog we are holding on a peer's behalf.
type Projection struct {
	NetworkID string
	CatalogID string
}

// RecordProjections notes that these catalogs were seen on this peer, and when
// our licence to keep them lapses.
//
// Upsert, because a catalog seen again is the same projection with a later
// expiry -- not a second one. Re-crawling is what keeps a projection alive, so
// this is also the refresh.
func (s *Store) RecordProjections(ctx context.Context, networkID string, catalogIDs []string, expiresAt time.Time) error {
	if len(catalogIDs) == 0 {
		return nil
	}
	// One statement per catalog, inside one transaction. A text[] parameter
	// would be fewer round trips, but it rests on how the driver maps a Go
	// []string onto a Postgres array; a peer returns a bounded number of
	// catalogs, so the round trips are affordable and this is certain.
	//
	// The transaction is what matters: a half-recorded pass would leave
	// catalogs published to discovery that nothing here can name, and those
	// are exactly the ones that can never be purged.
	const query = `
INSERT INTO peer_projection (network_id, catalog_id, expires_at, last_seen_at)
VALUES ($1, $2, $3, now())
    ON CONFLICT (network_id, catalog_id)
    DO UPDATE SET expires_at = EXCLUDED.expires_at, last_seen_at = EXCLUDED.last_seen_at`

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed
	for _, catalogID := range catalogIDs {
		if _, err := tx.ExecContext(ctx, query, networkID, catalogID, expiresAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ProjectionsFor lists the catalogs currently held from one peer.
//
// What a successful crawl is compared against: anything held but not returned
// has been withdrawn, and a withdrawal is evidence we can act on now rather
// than waiting out the ttl.
func (s *Store) ProjectionsFor(ctx context.Context, networkID string) ([]string, error) {
	const query = `SELECT catalog_id FROM peer_projection WHERE network_id = $1`
	return s.catalogIDs(ctx, query, networkID)
}

// ExpiredProjections lists everything whose licence has lapsed, across peers.
//
// This is the one obligation that holds even when nothing else happens: a peer
// we cannot reach stops being served when its ttl runs out, with no action from
// the peer and no successful crawl needed.
func (s *Store) ExpiredProjections(ctx context.Context, asOf time.Time) ([]Projection, error) {
	const query = `
SELECT network_id, catalog_id FROM peer_projection WHERE expires_at <= $1`
	rows, err := s.db.QueryContext(ctx, query, asOf)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var expired []Projection
	for rows.Next() {
		var p Projection
		if err := rows.Scan(&p.NetworkID, &p.CatalogID); err != nil {
			return nil, err
		}
		expired = append(expired, p)
	}
	return expired, rows.Err()
}

// ProjectedNetworks lists every peer we are holding anything from.
//
// Compared against the peers the registry still admits: a network here and not
// there has been suspended, and its projections have to go even though we will
// never crawl it again to find that out.
func (s *Store) ProjectedNetworks(ctx context.Context) ([]string, error) {
	const query = `SELECT DISTINCT network_id FROM peer_projection`
	return s.catalogIDs(ctx, query)
}

// ForgetProjection drops the record, after the catalog has been purged from
// discovery.
//
// Called only on success. A failed purge leaves the row, so the next sweep
// tries again -- forgetting first would leave a catalog served for ever with
// nothing left that could name it.
func (s *Store) ForgetProjection(ctx context.Context, networkID, catalogID string) error {
	const query = `DELETE FROM peer_projection WHERE network_id = $1 AND catalog_id = $2`
	_, err := s.db.ExecContext(ctx, query, networkID, catalogID)
	return err
}

// catalogIDs runs a single-text-column query.
func (s *Store) catalogIDs(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
