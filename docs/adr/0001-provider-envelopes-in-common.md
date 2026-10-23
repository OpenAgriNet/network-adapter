# ADR-0001 — Provider envelopes live in common, with capability hooks

**Status:** Accepted
**Date:** 2026-10-23

## Context

PM-KISAN does not take plain JSON. Its grievance service takes AES-GCM under a
fixed key and nonce with a service token inside the body; its chatbot (OTP)
service takes AES-128-CBC with a fresh key sent beside the ciphertext. Some
calls must first verify an OTP the farmer received. Mappings are published
files and must stay plain JSON, and must never carry the token.

## Decision

`internal/common/envelope` seals a mapped request and opens the answer:
`aesGcm` (fixed key and nonce, from env vars named in config) and
`aesCbcKeyInBand` (fresh key per request, `cipher@key`). It adds the service
token after the mapping runs. It can verify a challenge on the provider before
the guarded call (`otpVerifyPath`, `otpVerifyActions`), on its own time budget
(`otpVerifyTimeoutMs`). Its HTTP client refuses an off-host redirect, as
common's does.

What only a capability knows comes in as `envelope.Hooks`: where its payload
carries the challenge, and what its provider's "yes" looks like. Settings are a
provider block in the step's config, read by `envelope.Parse`; `envelope:` is
required whenever any `envelope*` setting is present, so a typo cannot send
plaintext.

## Alternatives considered

- **Inside the Grievance plugin.** The next provider with an encrypted
  upstream would have to import a package named for another capability or copy
  it.
- **The `encrypter`/`decrypter` plugins.** They do X25519 key agreement for
  Beckn payloads; PM-KISAN's fixed-key GCM and key-in-band CBC are neither.
- **Encryption in the mapping.** JSONata has no crypto, and the token would
  have to be in a public file.

## Consequences

The GCM nonce is fixed because PM-KISAN requires it. With one key and one
nonce, two ciphertexts give the XOR of their plaintexts and a ciphertext can
be forged; this envelope adds no confidentiality or integrity of its own and
TLS is the only protection. A nonce shorter than 12 bytes is refused. The CBC
key travels with the ciphertext, so that envelope is obfuscation; its token is
the only secret.
