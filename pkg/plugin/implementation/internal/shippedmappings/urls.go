// Package shippedmappings names where the provider mappings live: helmcharts,
// which deployments read. Tests fetch from here, so the tested mapping is the
// shipped one. A new provider is one more line below.
package shippedmappings

// Base is helmcharts' quick-start mappings folder on main.
const Base = "https://raw.githubusercontent.com/OpenAgriNet/helmcharts/main/quick-start/config/mappings"

// One folder per provider.
const (
	Agmarknet  = Base + "/agmarknet"
	Knowledge  = Base + "/knowledge"
	Mausamgram = Base + "/mausamgram"
	PMFBY      = Base + "/pmfby"
	PMKisan    = Base + "/pmkisan"
	POCRA      = Base + "/pocra"
)
