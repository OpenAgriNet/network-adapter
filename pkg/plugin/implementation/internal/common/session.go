// A session: the token a provider hands out on one call and expects back on
// the next, held in the deployment's cache between the two.
//
// Separate from auth.go because it is a different thing wearing the same
// clothes. A credential in auth.go belongs to the DEPLOYMENT -- one value for
// every request, read from the environment, good until an operator rotates it.
// A session belongs to ONE FARMER: bought by an OTP that cannot be spent
// twice, good for minutes, and wrong to show anyone else. Holding both in
// AuthProfile would let a provider's service key be looked up by transaction
// id, and a farmer's session be shared by every request the step serves.
package common

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/log"
	"github.com/beckn-one/beckn-onix/pkg/model"
)

// SessionProfile is one binding's session: where the key comes from, which
// action buys a token and which ones spend it.
type SessionProfile struct {
	// The binding key this block was written under.
	Binding string

	// Where in the Beckn payload the value that identifies this session sits.
	// Comma-separated paths, tried in order -- the transaction id is written
	// transaction_id by one Beckn version and transactionId by the next, and
	// a deployment should not need a different build for each.
	KeyAt string

	// StoreUnder names another binding whose sessions these are. AIF's
	// grievance capability spends what its scheme-application capability
	// bought, exactly as PM-KISAN's grievance spends its OTP.
	StoreUnder string

	// CaptureOn names the actions whose answer carries a token, SendOn the
	// ones that must present it. A binding can do either, or both.
	CaptureOn []string
	SendOn    []string

	// Where the token and its lifetime sit in the provider's answer. Both
	// matched without regard to case: AIF answers Token on one call and token
	// on another.
	TokenField string
	TTLField   string

	// How long a token lasts when the answer does not say, and how much of
	// that lifetime to give back so a token does not expire mid-flight.
	// Kept as written and parsed by validate, so a bad value is refused at
	// startup naming the binding rather than read as zero.
	TTLRaw       string
	TTLMarginRaw string
	TTL          time.Duration
	TTLMargin    time.Duration

	// Optional. A payload value that must not change for the life of the
	// session -- the beneficiary the OTP was sent to. Checked on every call
	// that spends the token, so a leaked transaction id alone cannot ask
	// about somebody else.
	BindAt string

	// Where the token goes on the wire. Defaulted to the Bearer convention.
	Header string
	Prefix string
}

// sessionCredential is the per-farmer header one request carries beside the
// provider's own credential. Zero means the request carries no session.
type sessionCredential struct{ Name, Value string }

// heldSession is what the cache holds. A struct rather than the bare token so
// the beneficiary check has something to check against.
type heldSession struct {
	Token string `json:"token"`
	Bind  string `json:"bind,omitempty"`
}

// sessionFields is the whole vocabulary of session settings, in one list for
// the same reason authFields is: a name and its assignment kept apart drift,
// and the silent direction is the setting that parses and is discarded.
var sessionFields = map[string]func(*SessionProfile, string){
	"sessionKeyAt":      func(p *SessionProfile, v string) { p.KeyAt = v },
	"sessionBindingKey": func(p *SessionProfile, v string) { p.StoreUnder = v },
	"sessionCaptureOn":  func(p *SessionProfile, v string) { p.CaptureOn = splitList(v) },
	"sessionSendOn":     func(p *SessionProfile, v string) { p.SendOn = splitList(v) },
	"sessionTokenField": func(p *SessionProfile, v string) { p.TokenField = v },
	"sessionTtlField":   func(p *SessionProfile, v string) { p.TTLField = v },
	"sessionTtl":        func(p *SessionProfile, v string) { p.TTLRaw = v },
	"sessionTtlMargin":  func(p *SessionProfile, v string) { p.TTLMarginRaw = v },
	"sessionBindAt":     func(p *SessionProfile, v string) { p.BindAt = v },
	"sessionHeader":     func(p *SessionProfile, v string) { p.Header = v },
	"sessionPrefix":     func(p *SessionProfile, v string) { p.Prefix = v },
}

// ParseProviderSessions builds one session profile per binding from a plugin's
// flattened settings, and returns everything it did not claim.
//
// The leftovers matter: ParseProviderAuth refuses any dashed setting it does
// not recognise, which is what turns a misspelled credential into a startup
// error instead of a provider rejecting us. So session settings have to be
// taken out of the map before it ever sees them.
//
// The split is on the FIRST dash, as in ParseProviderAuth: no setting name
// carries one, a binding key routinely does.
func ParseProviderSessions(config map[string]string) (map[string]*SessionProfile, map[string]string, error) {
	profiles := map[string]*SessionProfile{}
	rest := make(map[string]string, len(config))

	// Sorted so a config with two mistakes reports the same one every run.
	keys := make([]string, 0, len(config))
	for key := range config {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		field, binding, dashed := strings.Cut(key, "-")
		set, isSetting := sessionFields[field]
		if !isSetting {
			rest[key] = config[key]
			continue
		}
		if !dashed {
			// Step-wide, which a session cannot be: one step serves several
			// bindings and only some of them hold sessions.
			return nil, nil, fmt.Errorf(
				"%q is set for the whole step; a session belongs to one binding, "+
					"so it goes in a block named for the binding key", key)
		}
		if strings.TrimSpace(binding) == "" {
			return nil, nil, fmt.Errorf("%q names no binding after the dash", key)
		}

		profile, seen := profiles[binding]
		if !seen {
			profile = &SessionProfile{Binding: binding}
			profiles[binding] = profile
		}
		set(profile, config[key])
	}

	for _, binding := range sortedKeys(profiles) {
		if err := profiles[binding].validate(); err != nil {
			return nil, nil, err
		}
	}
	if len(profiles) == 0 {
		// Nil, not an empty map: most steps hold no session, and "none" is
		// what a Config written by hand in a test says too.
		return nil, rest, nil
	}
	return profiles, rest, nil
}

// validate refuses a half-written session at startup, where the operator can
// read the message, rather than on the first farmer's request.
func (p *SessionProfile) validate() error {
	if strings.TrimSpace(p.KeyAt) == "" {
		return fmt.Errorf("%s: sessionKeyAt must say where in the payload the session key sits", p.Binding)
	}
	if len(p.CaptureOn) == 0 && len(p.SendOn) == 0 {
		return fmt.Errorf("%s: a session needs sessionCaptureOn, sessionSendOn, or both", p.Binding)
	}
	if len(p.CaptureOn) > 0 && strings.TrimSpace(p.TokenField) == "" {
		return fmt.Errorf("%s: sessionTokenField must name the field the token arrives in", p.Binding)
	}

	var err error
	if p.TTL, err = optionalDuration(p.Binding, "sessionTtl", p.TTLRaw); err != nil {
		return err
	}
	if p.TTLMargin, err = optionalDuration(p.Binding, "sessionTtlMargin", p.TTLMarginRaw); err != nil {
		return err
	}
	if len(p.CaptureOn) > 0 && p.TTLField == "" && p.TTL == 0 {
		// Without one of the two there is no lifetime, and a session with no
		// lifetime is either kept forever or thrown away at once.
		return fmt.Errorf("%s: set sessionTtlField, sessionTtl, or both, so a captured token expires", p.Binding)
	}

	if p.Header == "" {
		p.Header = "Authorization"
		if p.Prefix == "" {
			p.Prefix = "Bearer"
		}
	}
	return nil
}

// optionalDuration reads a duration that may be absent.
func optionalDuration(binding, field, raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s: %s %q must be a duration such as 30m or 60s", binding, field, raw)
	}
	return value, nil
}

// sortedKeys returns a map's keys in order, so errors and log lines read the
// same way on every run.
func sortedKeys[T any](byKey map[string]T) []string {
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// sessionBindings names the bindings that configured a session, for the
// startup error that reports there is nowhere to keep one.
func sessionBindings(cfg *Config) []string {
	return sortedKeys(cfg.SessionByBinding)
}

// captures and sends report what this action does with the session. Written
// to tolerate a nil profile: most bindings have none, and the caller should
// not have to say so twice.
func (p *SessionProfile) captures(action string) bool {
	return p != nil && slices.Contains(p.CaptureOn, action)
}

func (p *SessionProfile) sends(action string) bool {
	return p != nil && slices.Contains(p.SendOn, action)
}

// storeUnder is the binding whose sessions these are: this one, unless the
// operator pointed at another.
func (p *SessionProfile) storeUnder() string {
	if p.StoreUnder != "" {
		return p.StoreUnder
	}
	return p.Binding
}

// cacheKey ties the session to one exchange. Namespaced by binding so two
// providers cannot read each other's, even on a shared Redis.
func (p *SessionProfile) cacheKey(beckn any) (string, error) {
	for _, path := range splitList(p.KeyAt) {
		if value := firstValueAt(beckn, path); value != "" {
			return "session|" + p.storeUnder() + "|" + value, nil
		}
	}
	return "", model.NewBadReqErr("", fmt.Errorf(
		"the request carries nothing at %s to tie the session to", p.KeyAt))
}

// sessionCredentialFor returns the header a call that spends a session must
// carry.
func (s *Step) sessionCredentialFor(ctx context.Context, p *SessionProfile, beckn any) (sessionCredential, error) {
	key, err := p.cacheKey(beckn)
	if err != nil {
		return sessionCredential{}, err
	}

	raw, err := s.cache.Get(ctx, key)
	if err != nil {
		// A miss and an unreachable cache are the same error from here, so the
		// log carries the difference: the caller is told to verify again
		// either way, and only an operator can tell a dead Redis from an
		// expired token.
		log.Warnf(ctx, "no session held for %s: %v", p.storeUnder(), err)
	}
	if strings.TrimSpace(raw) == "" {
		return sessionCredential{}, model.NewBadReqErr("", fmt.Errorf(
			"this request needs a verified session and there is none, "+
				"or it has expired; start the verification again"))
	}

	var held heldSession
	if err := json.Unmarshal([]byte(raw), &held); err != nil {
		return sessionCredential{}, fmt.Errorf("the held session could not be read: %w", err)
	}
	if held.Token == "" {
		return sessionCredential{}, model.NewBadReqErr("", fmt.Errorf(
			"the held session carries no token; start the verification again"))
	}

	// The session was bought for one beneficiary. Without this, a transaction
	// id is enough to ask about anybody.
	if p.BindAt != "" && held.Bind != "" {
		if asked := firstValueAt(beckn, p.BindAt); asked != "" && asked != held.Bind {
			return sessionCredential{}, model.NewBadReqErr("", fmt.Errorf(
				"this request is for a different beneficiary than the verified session"))
		}
	}

	return sessionCredential{Name: p.Header, Value: strings.TrimSpace(p.Prefix + " " + held.Token)}, nil
}

// keepSession holds what the provider just handed out, so the next call in
// this exchange can spend it.
func (s *Step) keepSession(ctx context.Context, p *SessionProfile, beckn, answer any) error {
	token := asText(lookupField(answer, p.TokenField))
	if token == "" {
		// Not a failure. AIF's two init calls are the same endpoint: the first
		// only sends the OTP and has no token to give.
		log.Debugf(ctx, "%s answered without %s, so there is no session to keep", p.Binding, p.TokenField)
		return nil
	}

	key, err := p.cacheKey(beckn)
	if err != nil {
		return err
	}

	ttl := p.TTL
	if p.TTLField != "" {
		if stated := asSeconds(lookupField(answer, p.TTLField)); stated > 0 {
			ttl = stated
		}
	}
	// The provider's clock started before we read the answer, and the next
	// call takes time to reach it. Expiring early costs one re-verification;
	// expiring late costs a call that fails for no visible reason.
	if ttl -= p.TTLMargin; ttl <= 0 {
		return fmt.Errorf("%s handed out a session that expires within the %v margin",
			p.Binding, p.TTLMargin)
	}

	held := heldSession{Token: token}
	if p.BindAt != "" {
		held.Bind = firstValueAt(beckn, p.BindAt)
	}
	encoded, err := json.Marshal(held)
	if err != nil {
		return fmt.Errorf("could not encode the session: %w", err)
	}

	// Failing here rather than answering. The OTP that bought this token
	// cannot be spent twice, so a session that was not stored means the next
	// call fails with nothing explaining why -- better that the caller is told
	// now, while they still have the screen that asked for the OTP.
	if err := s.cache.Set(ctx, key, string(encoded), ttl); err != nil {
		return fmt.Errorf("the verified session could not be held: %w", err)
	}
	log.Infof(ctx, "held a session for %s for %v", p.storeUnder(), ttl)
	return nil
}

// firstValueAt returns the first string the path reaches, or "".
func firstValueAt(node any, path string) string {
	for _, value := range ValuesAt(node, path) {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// lookupField walks a dotted path through a decoded answer, matching each
// segment WITHOUT regard to case.
//
// ValuesAt is exact, which is right for a Beckn payload: the spec fixes the
// spelling. A provider's own answer is not fixed by anything -- AIF returns
// Success and Message on most calls and success and message on one error --
// and an operator cannot write a config entry per casing a provider might use.
func lookupField(node any, path string) any {
	for _, segment := range strings.Split(path, ".") {
		fields, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		var found bool
		for name, value := range fields {
			if strings.EqualFold(name, segment) {
				node, found = value, true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return node
}

// asText reads a value a provider may have sent as a string or a number.
func asText(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

// asSeconds reads a stated lifetime, which arrives as a JSON number from one
// provider and a quoted one from the next.
func asSeconds(value any) time.Duration {
	seconds, err := strconv.ParseFloat(asText(value), 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds * float64(time.Second))
}
