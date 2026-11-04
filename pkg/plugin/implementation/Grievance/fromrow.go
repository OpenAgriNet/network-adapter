package Grievance

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/common"
)

// rowSettings names, per block of a registry row's encryption, the config
// setting each field is. One table, so a row and a config cannot drift apart.
var rowSettings = map[string]map[string]string{
	"aesGcm": {
		"serviceTokenRef": "envelopeTokenEnv", "serviceTokenField": "envelopeTokenField",
		"encryptedRequestField": "envelopeRequestField", "encryptedResponsePaths": "envelopeResponsePaths",
		"aesKeyRef": "envelopeKeyEnv", "aesNonceRef": "envelopeNonceEnv",
	},
	"aesCbcKeyInBand": {
		"serviceTokenRef": "envelopeTokenEnv", "serviceTokenField": "envelopeTokenField",
		"encryptedRequestField": "envelopeRequestField", "encryptedResponsePaths": "envelopeResponsePaths",
	},
	"otp": {
		"type": "otpEnvelope", "bindingKey": "otpBindingKey",
		"serviceTokenRef": "otpTokenEnv", "serviceTokenField": "otpTokenField",
		"encryptedRequestField": "otpRequestField", "encryptedResponsePaths": "otpResponsePaths",
		"actions": "otpActions",
	},
	"otpVerify": {"path": "otpVerifyPath", "actions": "otpVerifyActions", "timeoutMs": "otpVerifyTimeoutMs"},
	"followUp": {
		"path": "followUpPath", "answerField": "followUpField",
		"actions": "followUpActions", "timeoutMs": "followUpTimeoutMs",
	},
}

// cipherBlocks are the blocks type chooses between; only the chosen one is read.
var cipherBlocks = map[string]bool{"aesGcm": true, "aesCbcKeyInBand": true}

// EncryptionFromRow builds one binding's envelope from its registry row, through
// the same parser as the plugin config, reading its keys from secrets.
func EncryptionFromRow(bindingKey string, encryption map[string]any, secrets common.Secrets) (common.Envelope, map[string]string, error) {
	keys, err := encryptionKeys(bindingKey, encryption)
	if err != nil {
		return nil, nil, err
	}
	hooks := pmkisanHooks
	hooks.Secrets = secrets
	parsed, _, err := parseEnvelopes(keys, hooks)
	if err != nil {
		return nil, nil, err
	}
	return parsed.Envelopes[bindingKey], parsed.HostedElsewhere[bindingKey], nil
}

// encryptionKeys writes a row's encryption block as the flat settings the
// plugin config carries for bindingKey.
func encryptionKeys(bindingKey string, encryption map[string]any) (map[string]string, error) {
	kind, _ := encryption["type"].(string)
	if kind == "" {
		return nil, fmt.Errorf("%s: the encryption block has no type", bindingKey)
	}
	keys := map[string]string{"envelope-" + bindingKey: kind}

	// Sorted so a row with two mistakes reports the same one every time.
	blocks := make([]string, 0, len(encryption))
	for name := range encryption {
		blocks = append(blocks, name)
	}
	sort.Strings(blocks)
	for _, name := range blocks {
		if name == "type" || (cipherBlocks[name] && name != kind) {
			continue
		}
		if err := addBlock(keys, bindingKey, name, encryption[name]); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func addBlock(keys map[string]string, bindingKey, name string, raw any) error {
	settings, known := rowSettings[name]
	if !known {
		return fmt.Errorf("%s: encryption.%s is not a block this plugin knows", bindingKey, name)
	}
	block, isBlock := raw.(map[string]any)
	if !isBlock {
		return fmt.Errorf("%s: encryption.%s is not an object", bindingKey, name)
	}
	for field, value := range block {
		setting, known := settings[field]
		if !known {
			return fmt.Errorf("%s: encryption.%s.%s is not a setting this plugin knows", bindingKey, name, field)
		}
		text, err := settingText(value)
		if err != nil {
			return fmt.Errorf("%s: encryption.%s.%s %w", bindingKey, name, field, err)
		}
		keys[setting+"-"+bindingKey] = text
	}
	return nil
}

// settingText is a row value as the config would write it. Numbers arrive from
// JSON as floats; a timeout is a whole number of milliseconds.
func settingText(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case float64:
		if v != math.Trunc(v) {
			return "", fmt.Errorf("must be a whole number, got %v", v)
		}
		return strconv.FormatInt(int64(v), 10), nil
	default:
		return "", fmt.Errorf("must be text or a number, got %T", value)
	}
}
