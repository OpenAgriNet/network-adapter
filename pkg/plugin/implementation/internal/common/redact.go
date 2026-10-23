// Keeping credentials out of anything this package logs or returns.
package common

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common/util"
)

// redact removes a credential from an error's text.
//
// Go's transport errors quote the whole URL -- `Get "http://host/p?token=..."
// dial tcp: ...` -- so an unreachable host would write the credential into the
// log. Nothing else here puts a URL in a message.
func (s *Step) redact(err error) error {
	if err == nil {
		return nil
	}
	text := s.redactString(err.Error())
	if text == err.Error() {
		return err
	}
	return redactedErr{text: text, err: err}
}

// redactedErr reports the redacted text from Error() and the original from
// Unwrap(), so errors.Is and errors.As still match.
//
// Two simpler things do not work. errors.New(text) breaks the chain.
// fmt.Errorf("%s: %w", text, err) restores it and undoes the redaction, because
// %w formats the original.
type redactedErr struct {
	text string
	err  error
}

func (e redactedErr) Error() string { return e.text }

func (e redactedErr) Unwrap() error { return e.err }

// explainRedacted renders a failed body for a human, credential removed.
//
// Redacted BEFORE Explain truncates, not after: a secret that straddles
// Explain's cut point would otherwise survive as a partial, unmatched prefix
// no substring replacement can find once the rest of it is gone.
func (s *Step) explainRedacted(body []byte) string {
	return util.Explain([]byte(s.redactString(string(body))))
}

// redactString removes every configured credential from text about to be logged
// or returned -- an error, a response body, or a requested URL.
//
// Every scheme, not only query. A provider quoting the request it rejected is
// the ordinary shape of a 401 body, and gateways echo the Authorization header,
// so a credential reaches a message under any scheme.
func (s *Step) redactString(text string) string {
	for _, secret := range s.secretForms() {
		text = strings.ReplaceAll(text, secret, util.RedactedMarker)
	}
	return text
}

// secretForms returns every credential this step could leak, longest first so a
// value containing another is replaced before its substring.
//
// EVERY profile, not the one being served: an error raised while serving one
// provider can quote another's credential. Sorting spans the merged set for the
// same reason -- one provider's token can be a substring of another's.
func (s *Step) secretForms() []string {
	var forms []string
	for _, auth := range s.auth {
		forms = append(forms, auth.secretForms()...)
	}
	return longestFirst(forms)
}

// secretForms returns every form this profile's credential can appear in.
//
// Per scheme, because redacting only the value we hold is not enough:
//
//   - basic: the wire form is base64(user:pass), so the password alone never
//     appears in an echoed header.
//   - query: Encode escapes, so a token with "+" or "=" appears as "%2B", "%3D".
//     Escaping what we hold uses the same function, so the two always agree.
//   - header: sent as-is.
//
// The raw form is kept too, for an error built from config rather than from the
// request.
func (a *authenticator) secretForms() []string {
	switch a.cfg.Scheme {
	case util.AuthSchemeBasic:
		username, password := os.Getenv(a.cfg.UsernameEnv), os.Getenv(a.cfg.PasswordEnv)
		if password == "" {
			return nil
		}
		forms := []string{password}
		if username != "" {
			// The wire form, which is what a gateway echoes back.
			forms = append(forms,
				base64.StdEncoding.EncodeToString([]byte(username+":"+password)))
		}
		// The username is NOT redacted: it identifies rather than
		// authenticates, and words like "user" or "admin" would eat
		// unrelated text.
		return forms
	case util.AuthSchemeHeader:
		value := os.Getenv(a.cfg.HeaderValueEnv)
		if value == "" {
			return nil
		}
		return []string{value}
	case util.AuthSchemeOAuth2:
		// Both halves: the secret we send the issuer, and the token it gave
		// back. The token is what reaches the provider.
		var forms []string
		if secret := os.Getenv(a.cfg.ClientSecretEnv); secret != "" {
			forms = append(forms, secret)
		}
		// Read without tokenMu: this is reached from inside the exchange, which
		// holds it.
		if held := a.token.Load(); held != nil && held.value != "" {
			forms = append(forms, held.value)
		}
		// The client id is NOT redacted: it identifies, it does not
		// authenticate.
		return forms
	case util.AuthSchemeQuery:
		value := os.Getenv(a.cfg.QueryValueEnv)
		if value == "" {
			return nil
		}
		forms := []string{value}
		if encoded := url.QueryEscape(value); encoded != value {
			forms = append(forms, encoded)
		}
		return forms
	case util.AuthSchemeTokenQuery, util.AuthSchemeTokenHeader:
		// oauth2's two halves: the secret goes to the token endpoint, and the
		// token it returns travels in a query string under tokenQuery or a
		// header under tokenHeader. Both forms are covered for both schemes --
		// the escaped one matters in a query string, where a token containing
		// "+" or "=" would otherwise survive redaction, and covering it on
		// tokenHeader as well costs nothing and cannot miss.
		var forms []string
		if secret := os.Getenv(a.cfg.TokenSecretEnv); secret != "" {
			forms = append(forms, secret)
		}
		// Read without tokenMu: reached from inside the exchange, which holds it.
		if held := a.token.Load(); held != nil && held.value != "" {
			forms = append(forms, held.value)
			if encoded := url.QueryEscape(held.value); encoded != held.value {
				forms = append(forms, encoded)
			}
		}
		// tokenUserField's value is NOT redacted, for the same reason a client
		// id is not: it identifies, it does not authenticate.
		return forms
	}
	return nil
}

// longestFirst orders replacement candidates so a longer form is substituted
// before any shorter one it contains.
func longestFirst(forms []string) []string {
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	return forms
}

// minSentValue is the shortest sent value worth masking. A provider's message
// is read by a person, and masking every short code -- a category "1", a
// season "2" -- would shred the sentence around the data it was meant to
// protect. Everything the packs mark no-log is far longer: a mobile is ten
// digits, an enrolment id twenty-one, a complaint a sentence.
const minSentValue = 4

// redactSent masks, in text a provider sent back, every value this adapter
// sent it.
//
// A provider quoting the request it refused is the ordinary shape of a
// rejection, and what we send carries the farmer's mobile, enrolment id and
// complaint -- fields the capability packs mark no-log, no-trace. redactString
// covers credentials, which are configured; this covers the request's own
// data, which is different on every call and so cannot be.
//
// Only what was actually SENT, not the whole Beckn payload: masking every
// string in the envelope would eat the capability name, the provider id and
// the action, leaving a log line that protects nothing and explains nothing.
// Nothing is lost by masking it -- it is what we sent, so it is already known.
func redactSent(text string, sent []byte) string {
	if text == "" || len(sent) == 0 {
		return text
	}
	var payload any
	if json.Unmarshal(sent, &payload) != nil {
		return text
	}
	// Longest first, so a value containing another is replaced before its
	// substring -- the same reason secretForms sorts.
	for _, value := range longestFirst(sentValues(payload, nil)) {
		text = strings.ReplaceAll(text, value, util.RedactedMarker)
	}
	return text
}

// sentValues collects every scalar in a sent request, as the text a provider
// would quote it back as. Numbers included: an id mapped as a number echoes as
// digits, and digits are what identify a farmer.
func sentValues(node any, found []string) []string {
	switch value := node.(type) {
	case map[string]any:
		for _, child := range value {
			found = sentValues(child, found)
		}
	case []any:
		for _, child := range value {
			found = sentValues(child, found)
		}
	case string:
		if len(value) >= minSentValue {
			found = append(found, value)
		}
	case json.Number:
		if text := value.String(); len(text) >= minSentValue {
			found = append(found, text)
		}
	case float64:
		// encoding/json's default for every number. %v gives 9.876543210e+09
		// for a mobile, which is not how a provider would quote it, so the
		// integral case is formatted as the digits it was sent as.
		if text := strconv.FormatFloat(value, 'f', -1, 64); len(text) >= minSentValue {
			found = append(found, text)
		}
	}
	return found
}
