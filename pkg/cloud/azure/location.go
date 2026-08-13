package azure

import azurepricing "github.com/TypeOneLabs/tellury/pkg/pricing/azure"

// locationRegion is the location node's answer to "what place is this" and the
// ONLY Azure-specific location canonicalisation point. Azure returns location
// in more than one form — Resource Graph uses ARM region names ("westeurope",
// "eastus") while some payloads and user-facing values use display names
// ("West Europe", "East US"). Every Azure ingestion path that stamps
// Node.Location or indexes a price row must call this wrapper; no Azure SDK
// field may bypass it.
//
// The implementation lives in pkg/pricing/azure.LocationRegion so the graph
// and the price catalogue share one function; this wrapper keeps the call
// site provider-local. Examples:
//
//	"West Europe" -> "westeurope"
//	"westeurope"   -> "westeurope"
//	"East US"      -> "eastus"
//	"UK South"     -> "uksouth"
//	"global"       -> "global"
func locationRegion(location string) string {
	return azurepricing.LocationRegion(location)
}
