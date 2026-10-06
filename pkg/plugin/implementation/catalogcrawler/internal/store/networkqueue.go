package store

// networkqueue.go — the cross-network half of the same queue.
//
// It shares crawler_queue and crawler_catalog with the local crawl, and the
// same claim/retry/park/complete semantics. What differs is the KEYSPACE and
// where content comes from:
//
//   local         network_id = ''. crawlmanager claims the row and re-fetches
//                 the catalog's index URL, so nothing is stored between poll
//                 and sync.
//   cross-network network_id = the network it came from. A discover answers
//                 with the catalogs themselves and there is no per-catalog URL
//                 to go back to, so the document is staged and the worker reads
//                 it from there.
//
// Two keyspaces because a catalog id is unique within one network and not
// across them: two networks fronting the same upstream publish the same id.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// StagedCatalog is one catalog waiting to be published, with what it needs to
// be published correctly.
type StagedCatalog struct {
	NetworkID string
	CatalogID string
	// Audience is what this copy is addressed to -- the networkId the discover
	// asked for. Carried because a discover response has no visibleTo of its
	// own: it is a publish directive and is never returned.
	Audience string
	Document json.RawMessage
}

// StageAndEnqueue records one catalog's content and queues it for publishing,
// in one transaction.
//
// Together, because either alone is a leak: a staged row nothing queued is
// never published and never cleaned up, and a queued row with no staged content
// is claimed by a worker that finds nothing to send.
//
// Re-staging overwrites. The crawl just fetched this catalog's current content
// from the network that owns it, so a copy held from an earlier pass is stale
// by definition.
func (s *Store) StageAndEnqueue(ctx context.Context, staged StagedCatalog, discoverURL string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: StageAndEnqueue begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO crawler_staged_catalog (network_id, catalog_id, audience, document, fetched_at)
		 VALUES ($1,$2,$3,$4, now())
		 ON CONFLICT (network_id, catalog_id) DO UPDATE SET
		   audience   = EXCLUDED.audience,
		   document   = EXCLUDED.document,
		   fetched_at = now()`,
		staged.NetworkID, staged.CatalogID, staged.Audience, []byte(staged.Document)); err != nil {
		return fmt.Errorf("store: StageAndEnqueue staging %s: %w", staged.CatalogID, err)
	}

	// Same coalescing rule as the local Enqueue: a row already in progress is
	// left claimed so a re-stage cannot yank it from the worker holding it.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO crawler_queue
		   (network_id, catalog_id, index_url, from_version, to_version, entry_version, op, status, attempts, next_attempt_at, claimed_at, claim_id, enqueued_at)
		 VALUES ($1,$2,$3,0,0,0,'sync','queued',0, now(), NULL, NULL, now())
		 ON CONFLICT (network_id, catalog_id) DO UPDATE SET
		   index_url       = EXCLUDED.index_url,
		   status          = CASE WHEN crawler_queue.claimed_at IS NULL THEN 'queued' ELSE crawler_queue.status END,
		   attempts        = CASE WHEN crawler_queue.claimed_at IS NULL THEN 0 ELSE crawler_queue.attempts END,
		   next_attempt_at = CASE WHEN crawler_queue.claimed_at IS NULL THEN now() ELSE crawler_queue.next_attempt_at END,
		   parked_at       = CASE WHEN crawler_queue.claimed_at IS NULL THEN NULL ELSE crawler_queue.parked_at END,
		   abandoned_at    = CASE WHEN crawler_queue.claimed_at IS NULL THEN NULL ELSE crawler_queue.abandoned_at END`,
		staged.NetworkID, staged.CatalogID, discoverURL); err != nil {
		return fmt.Errorf("store: StageAndEnqueue queueing %s: %w", staged.CatalogID, err)
	}

	return tx.Commit()
}

// ClaimedStaged is one claimed cross-network item, with its content already
// read -- so the worker never has to go back for it.
type ClaimedStaged struct {
	ID       string
	ClaimID  string
	Attempts int
	StagedCatalog
}

// ClaimNextStaged claims the next ready cross-network item and returns it with
// its staged document, or nil when there is nothing to do.
//
// The same atomicity as ClaimNext -- FOR UPDATE SKIP LOCKED, a fresh claim_id,
// and a lease so a crashed worker's row becomes reclaimable. Scoped to rows a
// network_id was recorded for, which is what keeps it off the local crawl's.
//
// A queue row whose staged content is gone is claimed and returned with an
// empty Document; the caller settles it rather than leaving it to be claimed
// again for ever.
func (s *Store) ClaimNextStaged(ctx context.Context) (*ClaimedStaged, error) {
	var (
		item     ClaimedStaged
		claimID  sql.NullString
		audience sql.NullString
		document []byte
	)
	err := s.db.QueryRowContext(ctx,
		`UPDATE crawler_queue
		    SET claimed_at = now(), status = 'in_progress', claim_id = gen_random_uuid()
		  WHERE id = (
		    SELECT id FROM crawler_queue
		     WHERE network_id <> ''
		       AND (claimed_at IS NULL OR claimed_at < now() - $1::interval)
		       AND next_attempt_at <= now()
		     ORDER BY next_attempt_at
		     FOR UPDATE SKIP LOCKED
		     LIMIT 1)
		 RETURNING id, claim_id, network_id, catalog_id,
		   (SELECT audience FROM crawler_staged_catalog c
		     WHERE c.network_id = crawler_queue.network_id AND c.catalog_id = crawler_queue.catalog_id),
		   (SELECT document FROM crawler_staged_catalog c
		     WHERE c.network_id = crawler_queue.network_id AND c.catalog_id = crawler_queue.catalog_id),
		   attempts`,
		claimLease.String()).
		Scan(&item.ID, &claimID, &item.NetworkID, &item.CatalogID, &audience, &document, &item.Attempts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: ClaimNextStaged: %w", err)
	}
	item.ClaimID, item.Audience = claimID.String, audience.String
	item.Document = json.RawMessage(document)
	return &item, nil
}

// CompleteStaged settles a published item: the content is dropped and the queue
// row removed, in one transaction.
//
// The staged document goes because it has served its purpose -- it exists only
// to carry content from the crawl to the worker. The next pass stages a fresh
// copy.
func (s *Store) CompleteStaged(ctx context.Context, item *ClaimedStaged) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: CompleteStaged begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM crawler_staged_catalog WHERE network_id = $1 AND catalog_id = $2`,
		item.NetworkID, item.CatalogID); err != nil {
		return fmt.Errorf("store: CompleteStaged dropping content: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM crawler_queue WHERE id = $1 AND claim_id = $2`,
		item.ID, item.ClaimID); err != nil {
		return fmt.Errorf("store: CompleteStaged settling claim: %w", err)
	}
	return tx.Commit()
}

// RescheduleStaged releases a claim and gates the retry behind nextAttemptAt.
// The staged content is LEFT in place: it is what the retry will publish.
func (s *Store) RescheduleStaged(ctx context.Context, id, claimID string, nextAttemptAt time.Time) error {
	return s.Reschedule(ctx, id, claimID, nextAttemptAt)
}

// ParkStaged releases and parks an item that failed permanently, leaving its
// content staged for whenever the park sweep revives it.
func (s *Store) ParkStaged(ctx context.Context, id, claimID string) error {
	return s.Park(ctx, id, claimID)
}
