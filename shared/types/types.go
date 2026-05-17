// Package types holds shared data structures passed between
// carvilon-server, mock, and shared helpers. Kept minimal in
// Saison 10 and grown as concrete needs emerge.
package types

// AdoptionAttrs mirrors the "attrs" sub-object in the adoption
// response that the mock sends back to UDM. Shared so the server
// side can build the same shape when issuing test fixtures.
type AdoptionAttrs struct {
	Adopted  string `json:"adopted"`
	AppVer   string `json:"app_ver"`
	Broker   string `json:"broker"`
	Firmware string `json:"fw"`
	IPv4     string `json:"ipv4"`
	MAC      string `json:"mac"`
	Revision string `json:"revision"`
	UAHID    string `json:"uah_id"`
}
