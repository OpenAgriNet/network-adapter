package KnowledgeAdvisory

import "github.com/beckn-one/beckn-onix/pkg/plugin/implementation/internal/upstream"

// prerequisites is what a knowledge advisory capability needs that its payload
// does not carry, keyed by binding key.
//
// Empty, and the reason is worth writing down because a token looks like a
// counter-example. This upstream sits behind an OAuth2 token endpoint, and
// "exchange a token" is exactly the kind of real I/O an entry here exists for
// -- but a token belongs in a request HEADER, and whatever a prerequisite
// returns reaches the mapping as _local and from there the request body or
// query string. There is no path from here to an Authorization header, and a
// credential handed to a published mapping file is a credential one expression
// away from being echoed into a request. So it is upstream's authScheme oauth2
// instead, where the value is read, used, and never passed on.
//
// An entry would be needed for something the payload cannot carry and the
// mapping cannot obtain: a free-text topic resolved to a governed taxonomy id,
// say. The corpus index is not one -- it is a deployment constant, and the
// mapping file is itself per-deployment, named by the registry row.
var prerequisites = upstream.Prerequisites{}
