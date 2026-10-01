package hetzner

// certSubjectsForContext computes the SAN list a context's cert
// should cover: the leaf FQDN <ctx>.<lbGroup>.<fqdnDomain> plus the
// wildcard below it for the hostnames routes actually use (e.g.
// keycloak-admin.<ctx>.<lbGroup>.local.test). It is the same name
// space defaultGatewayHostnamePattern opens the Gateway listener for;
// a cert that names anything else can never validate for a request
// the Gateway accepts.
//
// No IP SAN: in the shared-LB shape, the IP doesn't tell you which
// context the request is for (SNI does), and consumers always
// reach the cluster via FQDN via the dns-hint-ip /etc/hosts entry.
// Including the LB IPv4 would be a chicken-and-egg too -- the LB
// is created with the cert, so its IP isn't known when the cert
// is generated.
//
// Returns commonName (the leaf FQDN) + the DNS SAN slice for
// generateSelfSignedCert.
func certSubjectsForContext(context, lbGroup, fqdnDomain string) (commonName string, dnsNames []string) {
	if fqdnDomain == "" {
		fqdnDomain = "local.test"
	}
	commonName = context + "." + lbGroup + "." + fqdnDomain
	dnsNames = []string{commonName, "*." + commonName}
	return commonName, dnsNames
}
