// Package agmarket ports the collection and catalog-building logic proven out
// in tools/publish/mandi_publish (collect.go and build.go) into an importable
// package. The original tool lives in package main and cannot be imported
// directly; this file is a faithful port, not a redesign, and keeps the same
// reasoning in its comments because that reasoning comes from measured
// real-world defects in the upstream Agmarknet data, not from taste.
package agmarket

// India's bounding box, used only to catch coordinates that cannot be a mandi.
// Deliberately generous: this rejects obvious corruption, it does not decide
// whether a point is in the right district.
const (
	minLatitude, maxLatitude   = 6.0, 38.0
	minLongitude, maxLongitude = 68.0, 98.0
)

// Coordinate verdicts.
const (
	coordinateOK          = "ok"
	coordinateMissing     = "missing"
	coordinateSuspect     = "suspect"
	coordinateOutOfBounds = "outOfBounds"
)

// CatalogGeometryBudget caps how many geometries go into one catalog.
//
// The discovery service refuses to index past 256 geometries per catalog, and
// the limit is CUMULATIVE across publishes to the same catalogId -- measured
// 2026-09-11: batches of 200, 200 and 144 geometries into one catalog answered
// ACCEPTED, then PARTIAL with 144 errors, then PARTIAL with 288, each error
// count being cumulative minus 256. So a catalog is capped by the total
// geometry it will ever hold, and merging batches does not evade it.
//
// Only markets with a usable coordinate publish a geometry, so a state of
// mostly coordinate-less markets fits in one catalog however many markets it
// has.
const CatalogGeometryBudget = 256

// Market is one row of master data option 6.
//
// Latitude and Longitude are POINTERS so absent stays absent. The upstream
// leaves them null on 1176 of 3310 rows, and a float64 zero would present those
// markets as sitting at 0,0 -- a real place in the Gulf of Guinea.
type Market struct {
	MarketID     int      `json:"marketId"`
	MarketName   string   `json:"marketName"`
	StateName    string   `json:"stateName"`
	DistrictName string   `json:"districtName"`
	Latitude     *float64 `json:"latitude,omitempty"`
	Longitude    *float64 `json:"longitude,omitempty"`
}

// Commodity is one entry of a market's cmdt_details.
//
// Code is what the price call's commoditycode takes.
type Commodity struct {
	Code int    `json:"code"`
	Name string `json:"name"`
}

// StateMarket is one row of the market-commodity mapping: a market, its codes,
// and what it trades.
//
// Every code a select must carry is on this struct, which is the reason the
// tool prefers this call over reassembling the same facts from master data
// options 4, 5 and 6 by name.
type StateMarket struct {
	MarketID     int         `json:"marketId"`
	MarketName   string      `json:"marketName"`
	StateCode    string      `json:"stateCode"`
	StateName    string      `json:"stateName"`
	DistrictID   int         `json:"districtId"`
	DistrictName string      `json:"districtName"`
	Commodities  []Commodity `json:"commodities"`
}

// CollectedMarket is one market as this tool reports it: every code a select
// needs, the commodities it trades, and a coordinate the consumer can trust or
// a stated reason it cannot.
type CollectedMarket struct {
	MarketID     int    `json:"marketId"`
	MarketName   string `json:"marketName"`
	StateCode    string `json:"stateCode"`
	StateName    string `json:"stateName"`
	DistrictID   int    `json:"districtId"`
	DistrictName string `json:"districtName"`

	// Set only when CoordinateQuality is ok. A doubtful coordinate is reported
	// by its verdict rather than shipped for someone to use by accident.
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`

	CoordinateQuality string      `json:"coordinateQuality"`
	Commodities       []Commodity `json:"commodities"`
}

// coordinateQuality judges one market's coordinates.
//
// FIRST MATCH WINS, in the order below, so a pair that trips two rules reports
// one verdict deterministically rather than depending on check order.
//
// Nothing here repairs a coordinate. A wrong point returns the wrong mandi to a
// farmer and nothing downstream can detect it, so a doubtful pair is labelled
// and passed on for a human to decide about.
func coordinateQuality(lat, lon *float64) string {
	if lat == nil || lon == nil {
		return coordinateMissing
	}
	if *lat == *lon {
		// 31 rows upstream carry the same number in both fields. It is a
		// copy-paste defect, not a market on the 45-degree line -- and it
		// catches 0,0 too, which is why null island is suspect rather than
		// merely out of bounds.
		return coordinateSuspect
	}
	if *lat < minLatitude || *lat > maxLatitude || *lon < minLongitude || *lon > maxLongitude {
		return coordinateOutOfBounds
	}
	return coordinateOK
}

// joinMarkets attaches master-data coordinates to the mapping's rows.
//
// ON market_id, an exact integer match -- verified 273/273 for Maharashtra. The
// alternative, matching market and district NAMES across the two datasets,
// would have to cope with names that ship with trailing whitespace and would
// fail silently when it guessed wrong.
//
// A mapping row with no master counterpart is KEPT. It is a market the upstream
// says trades today, carrying every code a select needs; only its geometry is
// unknown, and that is what the verdict is for. Dropping it would lose a real
// market to a missing coordinate.
func joinMarkets(rows []StateMarket, markets []Market) []CollectedMarket {
	byID := make(map[int]Market, len(markets))
	for _, market := range markets {
		byID[market.MarketID] = market
	}

	out := make([]CollectedMarket, 0, len(rows))
	for _, row := range rows {
		collected := CollectedMarket{
			MarketID:     row.MarketID,
			MarketName:   row.MarketName,
			StateCode:    row.StateCode,
			StateName:    row.StateName,
			DistrictID:   row.DistrictID,
			DistrictName: row.DistrictName,
			Commodities:  row.Commodities,
		}

		master := byID[row.MarketID]
		collected.CoordinateQuality = coordinateQuality(master.Latitude, master.Longitude)
		if collected.CoordinateQuality == coordinateOK {
			collected.Latitude = master.Latitude
			collected.Longitude = master.Longitude
		}
		out = append(out, collected)
	}
	return out
}

// deriveQuality clears the coordinates on any market whose CoordinateQuality
// is not ok, so nothing downstream can accidentally use a bad coordinate.
func deriveQuality(markets []CollectedMarket) []CollectedMarket {
	out := make([]CollectedMarket, len(markets))
	copy(out, markets)
	for i := range out {
		if out[i].CoordinateQuality != coordinateOK {
			out[i].Latitude = nil
			out[i].Longitude = nil
		}
	}
	return out
}

// dedupeByMarketID collapses duplicate MarketID values to one entry, first
// occurrence wins, preserving order.
func dedupeByMarketID(markets []CollectedMarket) []CollectedMarket {
	seen := make(map[int]bool, len(markets))
	out := make([]CollectedMarket, 0, len(markets))
	for _, m := range markets {
		if seen[m.MarketID] {
			continue
		}
		seen[m.MarketID] = true
		out = append(out, m)
	}
	return out
}

// geometryCost is what one market spends against the budget: one Point when
// its coordinate survived the collector's verdict, nothing otherwise.
func geometryCost(market CollectedMarket) int {
	if market.CoordinateQuality == coordinateOK && market.Latitude != nil && market.Longitude != nil {
		return 1
	}
	return 0
}

// chunkMarkets splits markets into runs that each stay within the geometry
// budget, preserving order so the partition is deterministic and a market
// lands in exactly one chunk.
//
// A market costing nothing never forces a split; a run is cut only when adding
// the next market would exceed the budget.
func chunkMarkets(markets []CollectedMarket, budget int) [][]CollectedMarket {
	var chunks [][]CollectedMarket
	var current []CollectedMarket
	spent := 0

	for _, market := range markets {
		cost := geometryCost(market)
		if len(current) > 0 && spent+cost > budget {
			chunks = append(chunks, current)
			current, spent = nil, 0
		}
		current = append(current, market)
		spent += cost
	}
	if len(current) > 0 {
		chunks = append(chunks, current)
	}
	return chunks
}
