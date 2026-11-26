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
        {"key": "transactionId", "value": "{{$guid}}"},
        {"key": "messageId", "value": "{{$guid}}"},
        {"key": "timestamp", "value": "{{$isoTimestamp}}"},
    ]


def variabilise(document):
    """Replace the per-deployment values in a built document.

    The context's ids and timestamp, and every visibleTo network. The catalog
    and its resources are untouched: they are what the request exists to send.
    """
    context = document.get("context", {})
    for field, variable in CONTEXT_VARIABLES.items():
        if field in context:
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
            "slug": os.path.basename(path).removesuffix(".json").split("-")[-1],
            "catalogId": catalog["id"],
            "name": catalog["descriptor"]["name"],
            "resources": len(catalog["resources"]),
            "updateMode": directives[0].get("updateMode", "MERGE"),
            "document": document,
        })
    return states


def build(states, name, settings):
    folders, total_requests, total_resources = [], 0, 0

    for code in sorted(states):
        documents = sorted(states[code], key=lambda d: d["slug"])
        state_name = documents[0]["name"].split("—")[-1].strip()
        resources = sum(d["resources"] for d in documents)
        requests = []

        for document in documents:
            label = f"Publish - {state_name} [{document['catalogId'].split(':')[-1]}]"
            requests.append(publish_request(
                label, variabilise(document["document"]),
                document["catalogId"], document["resources"], document["updateMode"]))

        requests.append(discover_request(code, state_name, resources))
        total_requests += len(requests)
        total_resources += resources

        folders.append({
            "name": f"{state_name} ({code})",
            "description": (
                f"{resources} markets in {len(documents)} "
                f"catalog{'s' if len(documents) > 1 else ''}."
                + ("\n\nThis state exceeded the 256-geometry publish budget, so it is "
                   "split across several catalogIds. Run EVERY publish request here: "
                   "each one replaces only its own catalog."
                   if len(documents) > 1 else "")
            ),
            "item": requests,
        })

    return {
        "info": {
            "name": name,
            "description": (
                "MandiPrice onDemand catalogs, one folder per state, generated from "
                "the documents `catalog-publish` built.\n\n"
                "**Set `discoveryUrl` and `networkId`** in your environment first — "
                "a discover whose context omits `networkId` returns nothing at all.\n\n"
                "Publish requests use `updateMode: FULL`: a resource absent from the "
                "body is deleted from that catalog. Two of these states are split "
                "across several catalogIds; run all of a state's publish requests."
            ),
            "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json",
        },
        "item": folders,
        "variable": collection_variables(settings),
    }, total_requests, total_resources


def merge_into(path, folders, match, folder_name):
    """Splice the per-state folders into an existing collection.

    REPLACES any top-level folder whose name contains `match`, rather than
    appending: run twice and a collection would otherwise hold two MandiPrice
    folders, and the stale one looks exactly as authoritative as the new one.
    The replacement keeps the original folder's POSITION, so the collection
    does not reshuffle itself on every regeneration.
    """
    with open(path, encoding="utf-8") as handle:
        collection = json.load(handle)

    replacement = {
        "name": folder_name,
        "description": (
            "One folder per state, generated by scripts/catalog-publish/build-postman.py "
            "from the documents catalog-publish wrote. Do not hand-edit: it is "
            "regenerated wholesale.\n\n"
            "Publish uses `updateMode: FULL` — a resource absent from the body is "
            "deleted from that catalog. Two states are split across several "
            "catalogIds; run all of a state's publish requests."
        ),
        "item": folders,
    }

    at = next((i for i, item in enumerate(collection.get("item", []))
               if match.lower() in item.get("name", "").lower()), None)
    if at is None:
        collection.setdefault("item", []).append(replacement)
        print(f"  merged: appended {folder_name!r} (no folder matched {match!r})")
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
    parser.add_argument("--folder-name", default="MandiPrice Capability - Agmarknet (all states)",
                        help="name of the folder the merged requests go under")
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
        "networkId": args.network_id,
        "discoveryUrl": args.discovery_url,
        "senderId": args.sender_id,
        "receiverId": args.receiver_id,
    }

    if not args.out and not args.merge_into:
        sys.exit("give --out, --merge-into, or both")

    VARIABLE_DEFAULTS.extend(collection_variables(settings))

    states = read_documents(args.catalogs)
    collection, requests, resources = build(states, args.name, settings)

    written = []
    if args.out:
        with open(args.out, "w", encoding="utf-8") as handle:
            json.dump(collection, handle, indent=2, ensure_ascii=False)
        written.append(args.out)
    if args.merge_into:
        merge_into(args.merge_into, collection["item"], args.folder_match, args.folder_name)
        written.append(args.merge_into)

    target = written[0]
    size = os.path.getsize(target) / (1024 * 1024)
    print(" ".join(written))
    print(f"  states:    {len(states)}")
    print(f"  folders:   {len(collection['item'])}")
    print(f"  requests:  {requests}  ({requests - len(states)} publish, {len(states)} discover)")
    print(f"  resources: {resources}")
    print(f"  size:      {size:.1f} MiB")
    print("  variables:")
    for key in ("discoveryUrl", "networkId", "senderId", "receiverId"):
        print(f"    {key:<14} {settings[key] or '(empty — fill in Postman)'}")


if __name__ == "__main__":
    main()
