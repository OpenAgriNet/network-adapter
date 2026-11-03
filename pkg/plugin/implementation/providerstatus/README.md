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

It acts on two request paths and lets every other through untouched. The path
decides, not `context.action`, because the router picks the target from the
path: the last segment of the URL path is `publish` or `discover` under either
module path (`/` in the multi-adapter deployment, `/_internal/network/` in the
single-adapter one). The body's `context.action`, compared ignoring case and
surrounding spaces, must then agree with the path, or the request is rejected
with a 400: `publish` or `catalog/publish` on the publish path, `discover` on
the discover path. A missing action is a disagreement.

**`publish`** — every catalog in the batch must name a provider that is active.
If any one does not, the whole publish is rejected with a 400 naming that
provider, so a sender is never left guessing which catalogs landed. A catalog
that names no provider is rejected too: an unnamed provider cannot be checked.
So is a publish with no catalogs at all (a 400).

**`discover`** — the request's filter is narrowed to the active providers, so
inactive and unknown providers' catalogs are not returned. Nothing is deleted:
when a provider is set active again its catalogs show up again with no republish
or re-crawl. A request with no filter gets one. A request with a filter keeps
it, AND-ed with the allowlist. The filter must be a `jsonpath` filter (an empty
`type` counts as `jsonpath`) of the form `$.catalogs[*] ? ( ... )`. Filters that
use `/* comments */`, single quotes or backslashes outside double-quoted strings
are refused rather than risk misreading them.

**On both paths**, a body is rejected with a 400 when an object the step reads
(the root, `context`, `message`, `intent`, `filters`, each catalog and its
`provider`) holds a key that differs from one the step reads only by case or
Unicode folding, such as `ID` beside `id` or `Filters`. A publish holding the
same key twice in any object is rejected too. Different JSON readers resolve
such keys differently, so the step and the service behind it could otherwise
read different fields.

A body without a JSON `context` object is rejected with a 400 on every path the
step sees, including the ones it otherwise lets through.

## Fails closed

If the registry cannot be asked, the request fails with a plain 500. So does a
registry answer the step cannot use: a provider id that is not a valid id, or a
full page of active providers (the search asks for 1000 rows; a full page might
be truncated, so it is refused rather than read as complete). It is never
treated as "everyone is active", and it is not a 400, because the sender did
nothing wrong. If no provider is active, discover returns nothing.

The plugin also refuses to start when the configured `registry` cannot list
active providers, since the gate would otherwise let every provider through.
Today only the `sunbirdRegistry` plugin can.

## Cost

One extra registry list call per publish and per discover. Caching is off for
this list, so nothing is reused between requests. If measurement shows this
matters, add a cache on the registry side rather than in this step.

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

Both lines are needed. A step listed under `registrySteps` but missing from
`steps:` loads and does nothing. Add it to the network module in each
deployment: the module whose path is `/` in the multi-adapter deployment and
`/_internal/network/` in the single-adapter one.

It takes no parameters of its own. The build list is `install/build-plugins.sh`
and the plugin id list for the combined deployment is `config/onix/plugin.yaml`.

Deployed configs live in the helmcharts repo (`network-config.yaml`,
`network-adapter-config.yaml`) and change in a separate PR. Nothing is active
until they do.
