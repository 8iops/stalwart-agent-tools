package contract

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestProviderContractPreservesAuthorityBoundaries(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "provider.json"))
	if err != nil {
		t.Fatal(err)
	}
	provider, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if provider.Runtime.ProviderPlacement != "re8ch-atlantic" {
		t.Fatalf("provider placement drifted: %q", provider.Runtime.ProviderPlacement)
	}
	if provider.Runtime.ConsumerPlacement != "re8ch" {
		t.Fatalf("consumer placement drifted: %q", provider.Runtime.ConsumerPlacement)
	}
	for _, prohibited := range []string{
		"arbitrary-jmap-method",
		"arbitrary-dav-url",
		"provider-credential-return",
		"kubernetes-proxy-through-amongcluster",
	} {
		if !slices.Contains(provider.ProhibitedInterfaces, prohibited) {
			t.Fatalf("missing prohibited interface %q", prohibited)
		}
	}
	for _, dynamic := range []string{"service-instances", "user-bindings", "provider-tokens"} {
		if !slices.Contains(provider.GitOpsExcludes, dynamic) {
			t.Fatalf("dynamic state %q was not excluded from GitOps", dynamic)
		}
	}
}

func TestProviderContractRejectsTransportBroadening(t *testing.T) {
	provider := Provider{
		SchemaVersion: "stalwart-agent-tools.re8ch.com/v1alpha1",
		Provider:      "stalwart",
		ServiceClass:  "collaboration.stalwart.v1",
		Runtime:       Runtime{DefaultTransport: "amongcluster"},
		Authority:     Authority{ProviderProtocol: "8iops/stalwart-agent-tools"},
		Surfaces: []Surface{
			{ID: "user", ScopeRequired: true, Operations: []string{"mail"}},
			{ID: "organization", ScopeRequired: true, Operations: []string{"accounts"}},
			{ID: "platform", ScopeRequired: true, Operations: []string{"domains"}},
		},
	}
	if err := provider.Validate(); err == nil {
		t.Fatal("transport broadening was accepted")
	}
}
