// Keeping credentials out of anything this package logs or returns.
package common

import (
	"encoding/base64"
	"net/url"
	"os"
	"sort"
	"strings"
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

// redactString removes every configured credential from text about to be logged
// or returned -- an error, a response body, or a requested URL.
//
// Every scheme, not only query. A provider quoting the request it rejected is
// the ordinary shape of a 401 body, and gateways echo the Authorization header,
// so a credential reaches a message under any scheme.
func (s *Step) redactString(text string) string {
	for _, secret := range s.secretForms() {
		text = strings.ReplaceAll(text, secret, redactedMarker)
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
		// The username is NOT redacted: it identifies rather than
		// authenticates, and words like "user" or "admin" would eat
		// unrelated text.
		return forms
	case AuthSchemeHeader:
		value := os.Getenv(a.cfg.HeaderValueEnv)
		if value == "" {
			return nil
		}
		return []string{value}
	case AuthSchemeOAuth2:
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
