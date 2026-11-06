"""Compare two capture directories and report every meaningful difference.

    python3 compare.py golden golden-adapter

A plain file diff is not usable here: the reply carries a fresh timestamp on
every call, so two runs of the same correct service never match byte for byte.
This ignores that one field and nothing else, then reports differences by their
path in the document so a failure says which field is wrong.
"""

import json
import os
import sys

# Regenerated per call by the service, so it can never match across runs.
VOLATILE = {("context", "timestamp")}


def walk(a, b, path=()):
    if path in VOLATILE:
        return
    if type(a) is not type(b):
        yield path, f"{type(a).__name__} vs {type(b).__name__}"
        return
    if isinstance(a, dict):
        for k in a.keys() | b.keys():
            if k not in a:
                yield path + (k,), f"absent vs {b[k]!r}"
            elif k not in b:
                yield path + (k,), f"{a[k]!r} vs absent"
            else:
                yield from walk(a[k], b[k], path + (k,))
    elif isinstance(a, list):
        if len(a) != len(b):
            yield path, f"{len(a)} items vs {len(b)}"
            return
        for i, (x, y) in enumerate(zip(a, b)):
            yield from walk(x, y, path + (str(i),))
    elif a != b:
        yield path, f"{a!r} vs {b!r}"


def main(left, right):
    names = sorted(f for f in os.listdir(left) if f.endswith(".json"))
    if not names:
        sys.exit(f"no captures in {left}/")

    failed = 0
    for name in names:
        rpath = os.path.join(right, name)
        if not os.path.exists(rpath):
            print(f"  MISSING  {name}  not captured in {right}/")
            failed += 1
            continue
        diffs = list(walk(json.load(open(os.path.join(left, name))), json.load(open(rpath))))
        if not diffs:
            print(f"  match    {name}")
            continue
        failed += 1
        print(f"  DIFFERS  {name}  ({len(diffs)} field{'s' if len(diffs) > 1 else ''})")
        for p, what in diffs[:15]:
            print(f"             {'.'.join(p) or '(root)'}: {what}")
        if len(diffs) > 15:
            print(f"             and {len(diffs) - 15} more")

    print()
    if failed:
        print(f"{failed} of {len(names)} differ. {left}/ is the contract; {right}/ has to match it.")
    else:
        print(f"all {len(names)} match, ignoring the regenerated timestamp.")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main(*(sys.argv[1:3] or ["golden", "golden-adapter"])))
