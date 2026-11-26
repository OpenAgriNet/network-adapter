#!/usr/bin/env python3
"""Turn the documents catalog-publish wrote into a Postman collection.

One Publish request per DOCUMENT, not per state: a state over the geometry
budget is published as several catalogs (MH, MH-2), each its own catalogId,
and collapsing them into one request would publish a catalog under the wrong
id. Each state also gets one Discover request that finds exactly its own
markets.

Everything environment-specific is a {{variable}} rather than a literal, so
one collection serves local, dev and staging: a collection carrying
"oan.dev" in two thousand places is a collection that only ever works against
one deployment. The catalog bodies themselves are left exactly as the
pipeline built them -- those are the payload under test.

  python3 build-postman.py --catalogs /tmp/mandi-states/openagrinet-MandiPrice \\
      --out OpenAgriNet.MandiPrice-states.postman_collection.json
"""
import argparse
import glob
import json
import os
import sys

# The volatile fields every request re-generates, and the identities that
# differ per deployment. Replaced with Postman variables wherever they appear
# in a built document's context.
CONTEXT_VARIABLES = {
    "transactionId": "{{transactionId}}",
    "messageId": "{{messageId}}",
    "timestamp": "{{timestamp}}",
    "senderId": "{{senderId}}",
    "receiverId": "{{receiverId}}",
}

JSON_HEADER = [{"key": "Content-Type", "value": "application/json"}]

# Filled in by main() once the settings are resolved. A module-level list so
# merge_into can add the ones an existing collection is missing.
VARIABLE_DEFAULTS = []


def collection_variables(settings):
    return [
        {"key": "discoveryUrl", "value": settings["discoveryUrl"],
         "description": "Discovery service base URL; /publish and /discover are appended."},
        {"key": "networkId", "value": settings["networkId"],
         "description": "The network these catalogs are visible to. REQUIRED -- "
                        "publishing with the wrong one makes them invisible, and a "
                        "discover without it returns nothing."},
        {"key": "senderId", "value": settings["senderId"]},
        {"key": "receiverId", "value": settings["receiverId"]},
        {"key": "mandiStateCode", "value": settings["inlineState"],
         "description": "Which state `Discover - any state` asks about. "
                        "AP BI CG CH DL GJ GO HP HR JK KK KL MG MH MN MP NG OR "
                        "PB PC RJ TL TN TR UC UP WB"},
        {"key": "transactionId", "value": "{{$guid}}"},
        {"key": "messageId", "value": "{{$guid}}"},
        {"key": "timestamp", "value": "{{$isoTimestamp}}"},
    ]


def variabilise(document):
    """Replace the per-deployment values in a built document.

    The context's ids and timestamp, and every visibleTo network. The catalog
    and its resources are untouched: they are what the request exists to send.
    """
    context = document.setdefault("context", {})
    # Set rather than replace-if-present: the pipeline leaves senderId and
    # receiverId to the crawler's sink to stamp, so a built document has
    # neither, and a request body that omits them says nothing about who
    # published the catalog.
    for field, variable in CONTEXT_VARIABLES.items():
        context[field] = variable
    if "networkId" in context:
        context["networkId"] = "{{networkId}}"

    for directive in document.get("message", {}).get("publishDirectives", []):
        if "visibleTo" in directive:
            directive["visibleTo"] = ["{{networkId}}"]
    return document


def publish_request(name, document, catalog_id, resources, update_mode):
    return {
        "name": name,
        "request": {
            "method": "POST",
            "header": JSON_HEADER,
            "url": {
                "raw": "{{discoveryUrl}}/publish",
                "host": ["{{discoveryUrl}}"],
                "path": ["publish"],
            },
            "description": (
                f"Publishes `{catalog_id}` — {resources} resources, updateMode "
                f"`{update_mode}`.\n\n"
                f"`{update_mode}` means resources absent from this body are DELETED "
                "from that catalog. That is deliberate: a market the upstream stops "
                "listing must disappear rather than linger. Sending a partial body "
                "here therefore removes everything it omits."
            ),
            "body": {
                "mode": "raw",
                "raw": json.dumps(document, indent=2, ensure_ascii=False),
                "options": {"raw": {"language": "json"}},
            },
        },
        "response": [],
    }


def discover_request(state_code, state_name, resources):
    body = {
        "context": {
            "domain": "agriculture",
            "action": "discover",
            "version": "2.0.0",
            # Without this the query matches nothing at all: networkId is the
            # visibility gate, not a filter, and omitting it is the single
            # most common reason a correct-looking discover returns zero.
            "networkId": ["{{networkId}}"],
            "transactionId": "{{transactionId}}",
            "messageId": "{{messageId}}",
            "timestamp": "{{timestamp}}",
        },
        "message": {
            "intent": {
                "filters": {
                    "type": "jsonpath",
                    "expression": (
                        "$.catalogs[*].resources[*] ? "
                        f'(@.resourceAttributes.market.state == "{state_code}")'
                    ),
                }
            },
            "limit": 100,
            "offset": 0,
        },
    }
    return {
        "name": f"Discover - {state_name} ({state_code})",
        "request": {
            "method": "POST",
            "header": JSON_HEADER,
            "url": {
                "raw": "{{discoveryUrl}}/discover",
                "host": ["{{discoveryUrl}}"],
                "path": ["discover"],
            },
            "description": (
                f"Every market published for {state_name}. Expect "
                f"`pagination.total` = **{resources}**.\n\n"
                "`textSearch` is not supported by this service — discover through "
                "spatial or filters. Note also that `@`-prefixed fields need "
                'SQL/JSON-path quoting: `@.resourceAttributes."@type"`.'
            ),
            "body": {
                "mode": "raw",
                "raw": json.dumps(body, indent=2, ensure_ascii=False),
                "options": {"raw": {"language": "json"}},
            },
        },
        "response": [],
    }


def any_state_discover():
    """One discover that serves every state, through {{mandiStateCode}}.

    Discover bodies are small -- a filter expression and a page size -- so
    parameterising one is enough to reach all 27 states. It is the PUBLISH
    bodies that are large, which is why only one of those is kept inline.
    """
    body = {
        "context": {
            "domain": "agriculture",
            "action": "discover",
            "version": "2.0.0",
            "networkId": ["{{networkId}}"],
            "transactionId": "{{transactionId}}",
            "messageId": "{{messageId}}",
            "timestamp": "{{timestamp}}",
        },
        "message": {
            "intent": {
                "filters": {
                    "type": "jsonpath",
                    "expression": "$.catalogs[*].resources[*] ? "
                                  '(@.resourceAttributes.market.state == "{{mandiStateCode}}")',
                }
            },
            "limit": 100,
            "offset": 0,
        },
    }
    return {
        "name": "Discover - any state ({{mandiStateCode}})",
        "request": {
            "method": "POST",
            "header": JSON_HEADER,
            "url": {"raw": "{{discoveryUrl}}/discover",
                    "host": ["{{discoveryUrl}}"], "path": ["discover"]},
            "description": (
                "Set **`mandiStateCode`** to any published state and send: AP, BI, CG, "
                "CH, DL, GJ, GO, HP, HR, JK, KK, KL, MG, MH, MN, MP, NG, OR, PB, PC, "
                "RJ, TL, TN, TR, UC, UP, WB.\n\n"
                "The counts each should return are listed in the folder description.\n\n"
                "`textSearch` is not supported by this service -- discover through "
                "spatial or filters. `@`-prefixed fields need SQL/JSON-path quoting: "
                '`@.resourceAttributes."@type"`.'
            ),
            "body": {"mode": "raw", "raw": json.dumps(body, indent=2, ensure_ascii=False),
                     "options": {"raw": {"language": "json"}}},
        },
        "response": [],
    }


def read_documents(directory):
    """Every built document, grouped by state code, in slug order."""
    paths = sorted(glob.glob(os.path.join(directory, "*.json")))
    if not paths:
        sys.exit(f"no documents found in {directory}")

    states = {}
    for path in paths:
        with open(path, encoding="utf-8") as handle:
            document = json.load(handle)
        catalog = document["message"]["catalogs"][0]
        directives = document["message"].get("publishDirectives", [{}])
        resource = catalog["resources"][0]["resourceAttributes"]
        code = resource["market"]["state"]

        states.setdefault(code, []).append({
            # From the catalogId, not the filename: a split state's file is
            # `mandi-price-MH-2.json`, and splitting that on "-" yields "2",
            # which collides with every other state's second chunk.
            "slug": catalog["id"].rsplit(":", 1)[-1],
            "catalogId": catalog["id"],
            "name": catalog["descriptor"]["name"],
            "resources": len(catalog["resources"]),
            "updateMode": directives[0].get("updateMode", "MERGE"),
            "document": document,
        })
    return states


def build(states, name, settings, inline_state):
    """A LIGHT collection: one state's publish inline, discover for all of them.

    A publish body carries every market in the state, so inlining 27 of them
    makes a 6 MiB collection nobody can review or diff. One worked example is
    enough to show the shape; the other states' documents live beside it as
    files, and publish-states.sh sends them.
    """
    if inline_state not in states:
        sys.exit(f"--inline-state {inline_state!r} is not among the published states: "
                 + ", ".join(sorted(states)))

    documents = sorted(states[inline_state], key=lambda d: d["slug"])
    state_name = documents[0]["name"].split("\u2014")[-1].strip()
    requests = []

    for document in documents:
        label = f"Publish - {state_name} [{document['catalogId'].split(':')[-1]}]"
        requests.append(publish_request(
            label, variabilise(document["document"]),
            document["catalogId"], document["resources"], document["updateMode"]))

    requests.append(discover_request(
        inline_state, state_name, sum(d["resources"] for d in documents)))
    requests.append(any_state_discover())

    # What every state holds, so the parameterised discover above has
    # something to be checked against without carrying 27 bodies.
    rows = "\n".join(
        f"| `{code}` | {sum(d['resources'] for d in states[code])} | "
        f"{', '.join(d['catalogId'].split(':')[-1] for d in sorted(states[code], key=lambda x: x['slug']))} |"
        for code in sorted(states))
    total = sum(sum(d["resources"] for d in group) for group in states.values())

    folder = {
        "name": name,
        "description": (
            f"**{state_name} is the worked example** -- its publish body is here in "
            f"full. The other {len(states) - 1} states' documents are NOT inlined: "
            "27 publish bodies make a 6 MiB collection nobody can review. They live "
            "in `api-collection/mandi-states/`, and `publish-states.sh` there sends "
            "any or all of them.\n\n"
            "`Discover - any state` works for every state -- set `mandiStateCode`.\n\n"
            f"Publish uses `updateMode: FULL`: a resource absent from the body is "
            "DELETED from that catalog. A state split across several catalogIds must "
            "have all of them published -- each replaces only its own.\n\n"
            f"**{total} markets across {len(states)} states.**\n\n"
            "| state | markets | catalogs |\n|---|---|---|\n" + rows
        ),
        "item": requests,
    }
    return folder, len(requests), total


PUBLISH_SCRIPT = r"""#!/usr/bin/env bash
# Publish the MandiPrice state catalogs that are not inlined in the Postman
# collection.
#
# The documents beside this script carry {{variables}}, exactly as the
# collection's requests do, so one set of files serves every environment.
# This substitutes them and POSTs the result.
#
#   NETWORK_ID=oan.dev DISCOVERY_URL=http://127.0.0.1:8090 ./publish-states.sh
#   NETWORK_ID=oan.dev DISCOVERY_URL=http://127.0.0.1:8090 ./publish-states.sh MH GJ
#
# No arguments publishes every state. Named states publish only those, and a
# state split across several catalogs sends ALL of them: under FULL each
# replaces only its own catalogId, so sending one and not the other would
# leave half the state stale.
set -euo pipefail
cd "$(dirname "$0")"

: "${NETWORK_ID:?set NETWORK_ID -- a catalog published to the wrong network is invisible}"
: "${DISCOVERY_URL:?set DISCOVERY_URL -- discovery base address, /publish is appended}"
SENDER_ID="${SENDER_ID:-provider.oan.dev}"
RECEIVER_ID="${RECEIVER_ID:-discovery.oan.dev}"

files=()
if [ "$#" -gt 0 ]; then
  for code in "$@"; do
    # The second glob catches a split state's extra catalogs (MH-2).
    for f in "catalog-$code.json" "catalog-$code"-*.json; do
      [ -e "$f" ] && files+=("$f")
    done
  done
  if [ "${#files[@]}" -eq 0 ]; then
    echo "no documents matched: $*" >&2
    exit 1
  fi
else
  files=(catalog-*.json)
fi

ok=0
bad=0
for file in "${files[@]}"; do
  body=$(sed -e "s|{{networkId}}|$NETWORK_ID|g" -e "s|{{senderId}}|$SENDER_ID|g" -e "s|{{receiverId}}|$RECEIVER_ID|g" -e "s|{{transactionId}}|$(uuidgen)|g" -e "s|{{messageId}}|$(uuidgen)|g" -e "s|{{timestamp}}|$(date -u +%Y-%m-%dT%H:%M:%SZ)|g" "$file")

  answer=$(printf '%s' "$body" | curl -sS -X POST "$DISCOVERY_URL/publish" -H 'Content-Type: application/json' --data-binary @-)

  # ACCEPTED only. A PARTIAL means the catalog indexed with pieces missing,
  # which is a failure however healthy the 200 looks.
  if printf '%s' "$answer" | grep -q '"status":"ACCEPTED"'; then
    printf '  %-24s ACCEPTED\n' "$file"
    ok=$((ok + 1))
  else
    printf '  %-24s FAILED: %s\n' "$file" "$(printf '%s' "$answer" | head -c 200)" >&2
    bad=$((bad + 1))
  fi
done

echo
echo "published $ok, failed $bad"
[ "$bad" -eq 0 ]
"""


def write_states_dir(directory, states, inline_state, settings):
    """The per-state documents, as files, with a script that publishes them.

    Written with {{variables}} rather than resolved values so one directory
    serves every environment -- the same reason the collection's requests
    carry them.
    """
    os.makedirs(directory, exist_ok=True)
    for stale in glob.glob(os.path.join(directory, "catalog-*.json")):
        os.remove(stale)

    written = 0
    for code in sorted(states):
        for document in sorted(states[code], key=lambda d: d["slug"]):
            name = f"catalog-{document['slug']}.json"
            with open(os.path.join(directory, name), "w", encoding="utf-8") as handle:
                json.dump(variabilise(document["document"]), handle,
                          indent=2, ensure_ascii=False)
            written += 1

    script = os.path.join(directory, "publish-states.sh")
    with open(script, "w", encoding="utf-8") as handle:
        handle.write(PUBLISH_SCRIPT)
    os.chmod(script, 0o755)

    rows = "\n".join(
        f"| `{code}` | {sum(d['resources'] for d in states[code])} | "
        + ", ".join(f"`catalog-{d['slug']}.json`"
                    for d in sorted(states[code], key=lambda x: x["slug"]))
        + (" |" if code != inline_state else " | also inline in the collection |")
        for code in sorted(states))

    with open(os.path.join(directory, "README.md"), "w", encoding="utf-8") as handle:
        handle.write(f"""# MandiPrice state catalogs

The publish bodies for every state. They are here rather than in the Postman
collection because 27 of them inline made a 6 MiB collection nobody could
review or diff. The collection keeps `{inline_state}` as a worked example and a
`Discover - any state` request that reaches all of them.

Generated by `beckn-onix/scripts/catalog-publish/build-postman.py`. Do not
hand-edit: they are regenerated wholesale.

## Publishing

```sh
NETWORK_ID=oan.dev DISCOVERY_URL=http://127.0.0.1:8090 ./publish-states.sh
NETWORK_ID=oan.dev DISCOVERY_URL=http://127.0.0.1:8090 ./publish-states.sh MH GJ
```

No arguments publishes every state. Documents carry `{{{{variables}}}}`, which
the script substitutes, so one directory serves every environment.

Publishing goes **direct to discovery**, not through the provider adapter: the
adapter's `/publish` is MERGE-only and rejects the `FULL` these send.

`FULL` means a resource absent from the body is **deleted** from that catalog.
A state split across several catalogIds must have all of them published — each
replaces only its own, so sending one and not the other leaves half the state
stale. `publish-states.sh MH` handles this: it sends every document for the
state you name.

## What is here

| state | markets | documents |
|---|---|---|
{rows}

Import one into Postman by pasting it as a raw body to `{{{{discoveryUrl}}}}/publish`.
""")
    return written


def merge_into(path, folder, match):
    """Splice the per-state folders into an existing collection.

    REPLACES any top-level folder whose name contains `match`, rather than
    appending: run twice and a collection would otherwise hold two MandiPrice
    folders, and the stale one looks exactly as authoritative as the new one.
    The replacement keeps the original folder's POSITION, so the collection
    does not reshuffle itself on every regeneration.
    """
    with open(path, encoding="utf-8") as handle:
        collection = json.load(handle)

    replacement = folder

    at = next((i for i, item in enumerate(collection.get("item", []))
               if match.lower() in item.get("name", "").lower()), None)
    if at is None:
        collection.setdefault("item", []).append(replacement)
        print(f"  merged: appended {folder['name']!r} (no folder matched {match!r})")
    else:
        print(f"  merged: replaced {collection['item'][at]['name']!r} in place")
        collection["item"][at] = replacement

    # Collection-level variables the generated requests rely on. Added only
    # when absent: an existing collection may already define them, and its
    # value is the one the rest of that collection is written against.
    existing = {v["key"] for v in collection.get("variable", [])}
    for variable in VARIABLE_DEFAULTS:
        if variable["key"] not in existing:
            collection.setdefault("variable", []).append(variable)
            print(f"  added collection variable {variable['key']!r}")

    with open(path, "w", encoding="utf-8") as handle:
        json.dump(collection, handle, indent=2, ensure_ascii=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--catalogs", required=True,
                        help="directory of documents written by catalog-publish")
    parser.add_argument("--out", help="write a standalone collection here")
    parser.add_argument("--merge-into",
                        help="splice the per-state folders into an EXISTING collection, "
                             "replacing any folder whose name matches --folder-match")
    parser.add_argument("--folder-match", default="MandiPrice",
                        help="substring identifying the folder to replace when merging")
    parser.add_argument("--folder-name", default="MandiPrice Capability - Agmarknet",
                        help="name of the folder the merged requests go under")
    parser.add_argument("--inline-state", default="KK",
                        help="the one state whose publish body is kept in the collection")
    parser.add_argument("--states-dir",
                        help="write every state's document here, with a script to publish them")
    parser.add_argument("--name", default="OpenAgriNet MandiPrice - all states")

    # The VALUES the generated collection's variables default to. Arguments
    # rather than literals, defaulting from the same environment variables
    # catalog-publish itself reads, so one export drives both and a collection
    # built for staging cannot silently carry a local address.
    parser.add_argument("--network-id", default=os.environ.get("NETWORK_ID", ""),
                        help="default for the collection's {{networkId}} [$NETWORK_ID]")
    parser.add_argument("--discovery-url", default=os.environ.get("DISCOVERY_URL", ""),
                        help="default for {{discoveryUrl}} [$DISCOVERY_URL]")
    parser.add_argument("--sender-id", default=os.environ.get("SENDER_ID", ""),
                        help="default for {{senderId}} [$SENDER_ID]")
    parser.add_argument("--receiver-id", default=os.environ.get("RECEIVER_ID", ""),
                        help="default for {{receiverId}} [$RECEIVER_ID]")
    args = parser.parse_args()

    # networkId is the visibility gate: a collection shipped with an empty one
    # publishes catalogs nobody can see and discovers nothing. Everything else
    # can reasonably be filled in in Postman; this cannot be left to chance.
    if not args.network_id.strip():
        sys.exit("--network-id (or $NETWORK_ID) is required: a catalog published "
                 "to an empty network is invisible, and a discover without one "
                 "returns nothing")

    settings = {
        "inlineState": args.inline_state,
        "networkId": args.network_id,
        "discoveryUrl": args.discovery_url,
        "senderId": args.sender_id,
        "receiverId": args.receiver_id,
    }

    if not args.out and not args.merge_into:
        sys.exit("give --out, --merge-into, or both")

    VARIABLE_DEFAULTS.extend(collection_variables(settings))

    states = read_documents(args.catalogs)
    folder, requests, resources = build(states, args.folder_name, settings, args.inline_state)

    # The documents go out FIRST: build() has already rewritten the inline
    # state's document in place, and the directory must hold every state in
    # the same {{variable}} form, not one resolved and the rest not.
    documents = 0
    if args.states_dir:
        documents = write_states_dir(args.states_dir, states, args.inline_state, settings)

    collection = {
        "info": {
            "name": args.name,
            "description": folder["description"],
            "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
        },
        "item": folder["item"],
        "variable": collection_variables(settings),
    }

    written = []
    if args.out:
        with open(args.out, "w", encoding="utf-8") as handle:
            json.dump(collection, handle, indent=2, ensure_ascii=False)
        written.append(args.out)
    if args.merge_into:
        merge_into(args.merge_into, folder, args.folder_match)
        written.append(args.merge_into)

    target = written[0]
    size = os.path.getsize(target) / (1024 * 1024)
    print(" ".join(written))
    print(f"  inline state:  {args.inline_state} "
          f"({sum(d['resources'] for d in states[args.inline_state])} markets, "
          f"{len(states[args.inline_state])} catalog(s))")
    print(f"  requests:      {requests} in the collection")
    print(f"  states total:  {len(states)}  ({resources} markets)")
    if args.states_dir:
        print(f"  documents:     {documents} written to {args.states_dir}")
    print(f"  size:      {size:.1f} MiB")
    print("  variables:")
    for key in ("discoveryUrl", "networkId", "senderId", "receiverId"):
        print(f"    {key:<14} {settings[key] or '(empty — fill in Postman)'}")


if __name__ == "__main__":
    main()
