package main

import (
	"context"
	"encoding/json"
	"fmt"
)

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

// Markets fetches master data option 6.
func (c *Client) Markets(ctx context.Context, m mapperRunner, mappingBase, token string) ([]Market, error) {
	out, err := c.call(ctx, m, mappingBase+"/master-markets.yaml",
		"/v1/fetch-agmarknet-master-data", map[string]any{"token": token})
	if err != nil {
		return nil, err
	}
	var markets []Market
	if err := json.Unmarshal(out, &markets); err != nil {
		return nil, fmt.Errorf("market list could not be read: %w", err)
	}
	return markets, nil
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

// StateMarkets fetches one state's market-commodity mapping over a date window.
func (c *Client) StateMarkets(ctx context.Context, m mapperRunner,
	mappingBase, token, stateCode, fromDate, toDate string) ([]StateMarket, error) {
	out, err := c.call(ctx, m, mappingBase+"/market-commodity.yaml",
		"/v1/fetch-agmarknet-market-commodity-mapping", map[string]any{
			"token":     token,
			"stateCode": stateCode,
			"fromDate":  fromDate,
			"toDate":    toDate,
		})
	if err != nil {
		return nil, err
	}
	var rows []StateMarket
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("%s: market-commodity rows could not be read: %w", stateCode, err)
	}
	return rows, nil
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

// Window is the date range the market-commodity mapping was asked for, carried
// on the output because it decides which markets appear at all.
type Window struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// StateError records a state whose fetch failed, so a partial run says which
// part is missing rather than looking complete.
type StateError struct {
	StateCode string `json:"stateCode"`
	Reason    string `json:"reason"`
}

// Collection is the whole output document.
type Collection struct {
	GeneratedAt string            `json:"generatedAt"`
	Window      Window            `json:"window"`
	Markets     []CollectedMarket `json:"markets"`
	StateErrors []StateError      `json:"stateErrors"`

	// EmptyStates lists a state whose fetch SUCCEEDED but returned zero market
	// rows. Not fatal -- a state can genuinely have nothing trading in a given
	// window -- but suspicious enough that it must be visible rather than
	// silently indistinguishable from "this state truly has no markets".
	EmptyStates []string `json:"emptyStates"`
}

// join attaches master-data coordinates to the mapping's rows.
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
func join(rows []StateMarket, markets []Market) []CollectedMarket {
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
