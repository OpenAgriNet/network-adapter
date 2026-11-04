// The settings that point a provider step at the secret store.
package common

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/secretstore"
)

// serviceAccountTokenFile is where Kubernetes mounts a pod's own token.
const serviceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// SecretStoreFrom builds the store a step's settings describe. No secretStoreUrl
// means no store, and registry rows' credentials are read from the environment.
//
// EXPORTED for the plugins that read their own config rather than ParseStepConfig.
func SecretStoreFrom(config map[string]string) (SecretStore, error) {
	url := strings.TrimSpace(config["secretStoreUrl"])
	if url == "" {
		return nil, nil
	}
	login, err := secretStoreLogin(config)
	if err != nil {
		return nil, err
	}
	var ttl time.Duration
	if raw := config["secretStoreCacheTtl"]; raw != "" {
		if ttl, err = time.ParseDuration(raw); err != nil || ttl <= 0 {
			return nil, fmt.Errorf("secretStoreCacheTtl %q is not a positive duration", raw)
		}
	}
	store, err := secretstore.New(secretstore.Config{
		URL: url, Path: config["secretStorePath"], Login: login, CacheTTL: ttl})
	if err != nil {
		return nil, err
	}
	return store, nil
}

func secretStoreLogin(config map[string]string) (secretstore.Login, error) {
	switch method := config["secretStoreLogin"]; method {
	case "approle":
		roleID, err := valueOfVariableNamedIn(config, "secretStoreRoleIdEnv")
		if err != nil {
			return nil, err
		}
		secretID, err := valueOfVariableNamedIn(config, "secretStoreSecretIdEnv")
		if err != nil {
			return nil, err
		}
		return secretstore.AppRole{RoleID: roleID, SecretID: secretID}, nil
	case "kubernetes":
		file := config["secretStoreTokenFile"]
		if file == "" {
			file = serviceAccountTokenFile
		}
		return secretstore.Kubernetes{Role: config["secretStoreRole"], TokenFile: file}, nil
	case "":
		return nil, errors.New("secretStoreLogin is required with secretStoreUrl: approle or kubernetes")
	default:
		return nil, fmt.Errorf("secretStoreLogin %q is not approle or kubernetes", method)
	}
}

// valueOfVariableNamedIn reads the environment variable a setting names: the
// AppRole ids live there, never in config.
func valueOfVariableNamedIn(config map[string]string, setting string) (string, error) {
	name := config[setting]
	if name == "" {
		return "", fmt.Errorf("%s is required for approle login", setting)
	}
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("%s names %s, which is not set", setting, name)
	}
	return value, nil
}
