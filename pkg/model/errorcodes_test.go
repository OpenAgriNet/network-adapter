package model

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// becknErrorCodes is the ErrorCode enum of the Beckn v2.0.0 spec
// (protocol-specifications-v2, core-v2.1.0-lts, api/v2.0.0/beckn.yaml).
var becknErrorCodes = []string{
	"CTX_MISSING_FIELD", "CTX_INVALID_FIELD", "CTX_ACTION_MISMATCH", "CTX_VERSION_UNSUPPORTED",
	"CTX_TTL_EXPIRED", "CTX_MESSAGE_ID_DUPLICATE", "CTX_TRANSACTION_ID_UNKNOWN",
	"CTX_CALLBACK_URL_MISSING", "CTX_INVALID_BPP_URI",
	"AUT_SIGNATURE_MISSING", "AUT_SIGNATURE_INVALID", "AUT_SUBSCRIBER_NOT_FOUND", "AUT_KEY_NOT_FOUND",
	"AUT_KEY_EXPIRED_OR_REVOKED", "AUT_UNAUTHORIZED_ACTION", "AUT_DOMAIN_NOT_ALLOWED",
	"AUT_REPLAY_DETECTED", "AUT_RATE_LIMITED",
	"SCH_INVALID_JSON", "SCH_SCHEMA_VALIDATION_FAILED", "SCH_REQUIRED_FIELD_MISSING",
	"SCH_FIELD_NOT_ALLOWED", "SCH_INVALID_ENUM", "SCH_INVALID_FORMAT", "SCH_INVALID_JSONPATH",
	"SCH_INVALID_JSONLD_CONTEXT", "SCH_TYPE_NOT_SUPPORTED", "SCH_ATTRIBUTE_PACK_INVALID",
	"SCH_INVALID_ENTITY_TYPE", "SCH_SCHEMA_ADAPTATION_FAILED",
	"NET_TIMEOUT", "NET_DOWNSTREAM_UNAVAILABLE", "NET_CALLBACK_FAILED", "NET_ACK_TIMEOUT",
	"NET_OVERLOADED", "NET_INTERNAL_ERROR", "NET_NOT_IMPLEMENTED", "NET_CATALOG_SOURCE_UNAVAILABLE",
	"NET_CATALOG_STALE",
	"BIZ_PROVIDER_NOT_FOUND", "BIZ_LOCATION_NOT_FOUND", "BIZ_CATEGORY_NOT_FOUND", "BIZ_ITEM_NOT_FOUND",
	"BIZ_OFFER_NOT_FOUND", "BIZ_ADDON_NOT_FOUND", "BIZ_ORDER_NOT_FOUND", "BIZ_QUANTITY_UNAVAILABLE",
	"BIZ_FULFILLMENT_UNAVAILABLE", "BIZ_QUOTE_EXPIRED_OR_CHANGED", "BIZ_PRICE_MISMATCH",
	"BIZ_PAYMENT_NOT_SUPPORTED", "BIZ_PAYMENT_FAILED", "BIZ_TRACKING_NOT_SUPPORTED",
	"BIZ_SUPPORT_NOT_AVAILABLE", "BIZ_NO_RESULTS_FOUND", "BIZ_INVALID_STATE_TRANSITION",
	"BIZ_ALREADY_CANCELLED", "BIZ_ALREADY_CONFIRMED", "BIZ_UPDATE_INCONSISTENT",
	"BIZ_INVALID_UPDATE_TARGET", "BIZ_INVALID_CANCELLATION_REASON", "BIZ_RATING_TARGET_NOT_FOUND",
	"BIZ_INVALID_RATING_VALUE", "BIZ_GENERIC_ERROR",
	"POL_CANCELLATION_NOT_ALLOWED", "POL_UPDATE_NOT_ALLOWED", "POL_GEO_RESTRICTED",
	"POL_ELIGIBILITY_NOT_MET", "POL_KYC_REQUIRED", "POL_CONSENT_REQUIRED", "POL_SANCTIONED_PARTY",
	"POL_CALLER_TRUST_INSUFFICIENT", "POL_NP_CAPACITY_EXCEEDED", "POL_NP_DORMANT",
	"POL_ENGAGEMENT_REFUSED", "POL_GENERIC_ERROR",
}

// adapterOnlyErrorCodes are the codes the adapter already answered with
// outside the Beckn enum. The list is closed: errors use an existing code.
var adapterOnlyErrorCodes = []string{
	"NET_ENTITY_NOT_FOUND", "SCH_ACTION_NOT_SUPPORTED", "SCH_SUBSCRIBER_NOT_FOUND", "BIZ_PUBLISH_FAILED",
}

var errorCodeLiteral = regexp.MustCompile(`"((?:CTX|AUT|SCH|NET|BIZ|POL)_[A-Z_]+)"`)

func TestSourceErrorCodes_AreExistingCodesOnly(t *testing.T) {
	allowed := map[string]bool{}
	for _, code := range append(becknErrorCodes, adapterOnlyErrorCodes...) {
		allowed[code] = true
	}

	err := filepath.WalkDir(repositoryRoot(t), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && (entry.Name() == ".git" || entry.Name() == "vendor") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range errorCodeLiteral.FindAllStringSubmatch(string(source), -1) {
			if !allowed[match[1]] {
				t.Errorf("%s uses %s, which is neither a Beckn error code nor one the adapter already had", path, match[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the source: %v", err)
	}
}

// repositoryRoot is the nearest directory above this package holding go.mod.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("finding the working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("no go.mod above the test's directory")
		}
		directory = parent
	}
}
