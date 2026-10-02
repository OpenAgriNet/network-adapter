# What a peer network may reach.
#
# Authentication happened before this ran: the adapter's signature step proved
# who the caller is, and a suspended peer's key resolves as unusable so the call
# never arrives. This policy does NOT re-check admission -- asking the same
# question twice, in the place with less information, is not defence in depth.
#
# It answers one thing: which actions may a peer ask for.
package federation.peer

import rego.v1

# The evaluator passes the WHOLE Beckn body as input, so the action is at
# input.context.action. An earlier draft read input.action, which is never set,
# and that made every call a violation.
default result := {"valid": true, "violations": []}

result := {
	"valid": count(violations) == 0,
	"violations": violations,
}

# Read-only, cross-network. A peer may look, and may narrow what it found.
# Anything that commits -- init, confirm, cancel -- is between the Consumer and
# the Provider, and is not one network's to authorise on another's behalf.
permitted := {"discover", "select"}

violations contains sprintf("action %q is not permitted across networks", [input.context.action]) if {
	not input.context.action in permitted
}
