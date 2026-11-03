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
`participantType` and `participantRole` (see Configuration) can narrow which
active participants count as providers.

It acts on two request paths and lets every other through untouched. The path
decides, not `context.action`, because the router picks the target from the
path: the last segment of the URL path is the publish or discover endpoint
(`publish` and `discover` unless configured) under either module path (`/` in
the multi-adapter deployment, `/_internal/network/` in the single-adapter one).
The body's `context.action`, compared ignoring case and surrounding spaces, must
then agree with the path, or the request is rejected with a 400: the endpoint
or `catalog/` + the endpoint on the publish path (`publish`, `catalog/publish`
by default), the endpoint on the discover path. A missing action is a
disagreement.

**`publish`** — every catalog in the batch must name a provider that is active.
The provider id is read at `providerField` inside each catalog (`provider.id`
by default).
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
(the root, `context`, `message`, `intent`, `filters`, each catalog and every
object along `providerField`) holds a key that differs from one the step reads only by case or
Unicode folding, such as `ID` beside `id` or `Filters`. A publish holding the
same key twice in any object is rejected too. Different JSON readers resolve
such keys differently, so the step and the service behind it could otherwise
read different fields.

A body without a JSON `context` object is rejected with a 400 on every path the
step sees, including the ones it otherwise lets through.

## Fails closed

If the registry cannot be asked, the request fails with a plain 500. So does a
registry answer the step cannot use: a provider id that is not a valid id. It is
never treated as "everyone is active", and it is not a 400, because the sender
did nothing wrong. If no provider is active, discover returns nothing.

The search does not override the registry's default page size. A registry that
caps its pages would silently shorten the list, so providers past the cap would
read as inactive: an availability risk, never a bypass.

The plugin also refuses to start when the configured `registry` cannot list
active providers, since the gate would otherwise let every provider through.
Today only the `sunbirdRegistry` plugin can. It refuses to start on an invalid
config too, including an unknown key, so a typo never falls back to a default.

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

Every config key is optional:

| Key | Default | Allowed |
|---|---|---|
| `publishEndpoint` | `publish` | `^[a-z][a-z0-9_]*$`, not equal to `discoverEndpoint` |
| `discoverEndpoint` | `discover` | `^[a-z][a-z0-9_]*$` |
| `providerField` | `provider.id` | dotted names, `^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$` (it is spliced into the jsonpath, so it stays this strict) |
| `participantType` | empty (any) | `node`, `upstream` |
| `participantRole` | empty (any) | `consumer`, `provider`, `network` |

A blank value for an endpoint or `providerField` is refused at startup, not
read as the default. So are crossed endpoints (`publishEndpoint: discover` or
`discoverEndpoint: publish`) and a `providerField` segment that differs from
another key the step reads only by case (`Message.id`, `Type`, `ID.id`).

The plugin cannot know the router's endpoint names, so a wrong endpoint value
(a typo, say) silently leaves the real endpoint ungated. The two endpoint
values must match the router's endpoint names exactly.

```yaml
plugins:
  registrySteps:
    - id: providerstatus
      config:
        providerField: bpp.provider.id
        participantRole: provider
```

The build list is `install/build-plugins.sh`
and the plugin id list for the combined deployment is `config/onix/plugin.yaml`.

Deployed configs live in the helmcharts repo (`network-config.yaml`,
`network-adapter-config.yaml`) and change in a separate PR. Nothing is active
until they do.
