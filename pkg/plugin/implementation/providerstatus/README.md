# Provider Status Plugin

A **registry step plugin** for Beckn-ONIX that keeps providers the registry does
not list as active out of publish and discover.

## Overview

A catalog is only as trustworthy as the provider behind it. Without this step, an
inactive or unregistered provider can still publish, and its catalogs stay
visible in discover. This step closes both gaps at the network adapter, so the
discovery service stays ignorant of the registry and needs no schema change.

It asks one question of the registry: which provider ids are active right now?
"Active" and "exists" are the same check. An id the registry does not list as
active is refused or hidden, whether it was deactivated or never registered.

It acts on two actions and ignores every other:

**`catalog/publish` and `publish`** — every catalog in the batch must name a
provider that is active. If any one does not, the whole publish is rejected with
a 400 naming that provider, so a sender is never left guessing which catalogs
landed. A catalog that names no provider is rejected too: an unnamed provider
cannot be checked.

**`discover`** — the request's filter is narrowed to the active providers, so
inactive and unknown providers' catalogs are not returned. Nothing is deleted:
when a provider is set active again its catalogs show up again with no republish
or re-crawl. A request with no filter gets one. A request with a filter keeps
it, AND-ed with the allowlist. The filter must be a `jsonpath` filter (an empty
`type` counts as `jsonpath`) of the form `$.catalogs[*] ? ( ... )`. Filters that
use `/* comments */` or single quotes outside double-quoted strings are refused
rather than risk misreading them.

## Fails closed

If the registry cannot be asked, the request fails with a plain 500. It is never
treated as "everyone is active", and it is not a 400, because the sender did
nothing wrong. If no provider is active, discover returns nothing.

The plugin also refuses to start when the configured `registry` cannot list
active providers, since the gate would otherwise let every provider through.

## Cost

One extra registry list call per publish and per discover. The registry plugin
runs with `cacheTTL: 0` today, so nothing is reused between requests. If
measurement shows this matters, add a cache on the registry side rather than in
this step.

## Configuration

Add the step to the network module's `registrySteps` and place it in `steps`
after `validateSign` and before `addRoute`:

```yaml
plugins:
  registrySteps:
    - id: providerstatus
steps:
  - validateSign
  - providerstatus
  - addRoute
```

It takes no parameters of its own. The build list is `install/build-plugins.sh`
and the plugin id list for the combined deployment is `config/onix/plugin.yaml`.

Deployed configs live in the helmcharts repo (`network-config.yaml`,
`network-adapter-config.yaml`) and change in a separate PR. Nothing is active
until they do.
