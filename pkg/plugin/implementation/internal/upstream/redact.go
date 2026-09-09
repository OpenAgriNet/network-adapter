// Keeping credentials out of anything this package logs or returns.
package upstream

import (
	"encoding/base64"
	"net/url"
	"os"
	"sort"
	"strings"
)

// redact removes a query-string credential from an error's text.
//
// Go's transport errors quote the whole URL -- `Get "http://host/p?token=..."
// dial tcp: ...` -- so without this, one unreachable host writes the credential
// into the log at warn level. Nothing else in this package puts a URL in a
// message, which is why this is the only place it is needed.
//
// A plain string replacement, because the value is what leaks and the value is
// what we hold. Parsing the error to find it would assume a shape net/http does
// not promise.
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

// redactedErr reports a redacted message while keeping the original reachable
// for errors.Is and errors.As.
//
// errors.New(text) was the obvious thing and it broke the chain: the redacted
// value is what gets %w-wrapped into the final 502, so under a query-string
// scheme -- and only then, since nothing else redacts -- errors.Is(err,
// context.DeadlineExceeded) silently stopped matching. Retry classification
// was never affected, because isPermanent tests the error before redaction,
// which is why nothing failed visibly.
//
// fmt.Errorf("%s: %w", text, err) would have restored the chain and undone the
// redaction with it: %w formats the original, credential included. Reporting
// the redacted text from Error() and the original from Unwrap() keeps both.
//
// The original's text is reachable through errors.Unwrap, which is a
// deliberate act by a caller who wants the cause -- and %v, %s and %w on the
// value itself all go through Error() and stay redacted.
type redactedErr struct {
	text string
	err  error
}

func (e redactedErr) Error() string { return e.text }

func (e redactedErr) Unwrap() error { return e.err }

// redactString removes the configured credential from any text about to be
// logged or returned -- an error, a provider's response body, or the URL that
// was requested.
//
// Logging those is deliberate: they say what was asked of whom and what came
// back, which is the first thing anyone wants when a provider misbehaves. This
// is what makes that safe to do at info and warn level.
//
// EVERY scheme, not just query. This used to return early unless the scheme was
// query, on the reasoning that only a query credential reaches a URL -- true of
// the URL, and wrong about the body. A provider quoting the request it rejected
// is the ordinary shape of a 401 or 403 body, an API gateway echoing the
// Authorization header is routine, and a wrong-credential 4xx is not retried,
// so it lands in the log once per request for as long as the credential is
// wrong. basic is the scheme the reference config ships.
func (s *Step) redactString(text string) string {
	for _, secret := range s.secretForms() {
		text = strings.ReplaceAll(text, secret, redactedMarker)
	}
	return text
}

// secretForms returns every form the configured credential can appear in,
// longest first so a value that contains another is replaced before its
// substring turns the longer one into a partial redaction.
//
// Per scheme, because the schemes leak differently and redacting the value we
// hold is not enough on its own:
//
//   - basic wraps the pair: SetBasicAuth sends base64(user:pass), so the
//     password alone does not appear on the wire and replacing it misses the
//     echoed header entirely.
//   - query escapes: authenticate goes through url.Values.Encode, so a base64
//     token carrying "+", "/" or "=" appears as "a%2Bb%2Fc%3D". Escaping what
//     we hold is exact -- same function Encode used, so the two agree by
//     construction rather than by a guess about which characters matter.
//   - header sends the value as-is.
//
// The raw form is kept alongside the wrapped one in both cases: an error built
// from the config rather than from the request still quotes the credential
// unwrapped.
func (s *Step) secretForms() []string {
	// EVERY profile, not the one being served.
	//
	// Redaction is about what could appear in a piece of text, not about which
	// provider a request happened to be for. An error or a log line built while
	// serving one provider can quote another's credential -- a shared client
	// echoing a header, an issuer naming the wrong caller -- and scoping this
	// to the active profile would let that reach the log unredacted. There are
	// a handful of profiles and this runs on a failure path, so walking all of
	// them costs nothing worth measuring.
	var forms []string
	for _, auth := range s.auth {
		forms = append(forms, auth.secretForms()...)
	}
	// Sorted across the merged set rather than per profile: one provider's
	// short token can be a substring of another's, and replacing the short one
	// first would leave the longer half-redacted.
	return longestFirst(forms)
}

// secretForms returns every form this profile's credential can appear in.
//
// Per scheme, because the schemes leak differently and redacting the value we
// hold is not enough on its own:
//
//   - basic wraps the pair: SetBasicAuth sends base64(user:pass), so the
//     password alone does not appear on the wire and replacing it misses the
//     echoed header entirely.
//   - query escapes: authenticate goes through url.Values.Encode, so a base64
//     token carrying "+", "/" or "=" appears as "a%2Bb%2Fc%3D". Escaping what
//     we hold is exact -- same function Encode used, so the two agree by
//     construction rather than by a guess about which characters matter.
//   - header sends the value as-is.
//
// The raw form is kept alongside the wrapped one in both cases: an error built
// from the config rather than from the request still quotes the credential
// unwrapped.
func (a *authenticator) secretForms() []string {
	switch a.cfg.Scheme {
	case AuthSchemeBasic:
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
		// The username is deliberately NOT redacted. It identifies rather than
		// authenticates, and it is routinely a short common word -- redacting
		// "user" or "admin" would eat unrelated text and cost the operator the
		// log line they came for. The pair and the password are the secrets.
		return forms
	case AuthSchemeHeader:
		value := os.Getenv(a.cfg.HeaderValueEnv)
		if value == "" {
			return nil
		}
		return []string{value}
	case AuthSchemeOAuth2:
		// Both halves: the client secret we send to the issuer, and the token
		// it gave back. The token is the one that reaches the provider, so it
		// is the one an echoing 401 body quotes.
		var forms []string
		if secret := os.Getenv(a.cfg.ClientSecretEnv); secret != "" {
			forms = append(forms, secret)
		}
		// Read without tokenMu: this is reached from inside the exchange, which
		// holds it.
		if held := a.token.Load(); held != nil && held.value != "" {
			forms = append(forms, held.value)
		}
		// The client id is deliberately NOT redacted: it identifies, it does
		// not authenticate, and it is what makes a log line useful.
		return forms
	case AuthSchemeQuery:
		value := os.Getenv(a.cfg.QueryValueEnv)
		if value == "" {
			return nil
		}
		forms := []string{value}
		if encoded := url.QueryEscape(value); encoded != value {
			forms = append(forms, encoded)
		}
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
