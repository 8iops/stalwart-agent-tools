package contract

import (
	"encoding/json"
	"fmt"
)

type Provider struct {
	SchemaVersion        string    `json:"schemaVersion"`
	Provider             string    `json:"provider"`
	ServiceClass         string    `json:"serviceClass"`
	SourceRepository     string    `json:"sourceRepository"`
	DeliveryRepository   string    `json:"deliveryRepository"`
	Runtime              Runtime   `json:"runtime"`
	Authority            Authority `json:"authority"`
	Surfaces             []Surface `json:"surfaces"`
	ActivationClasses    []string  `json:"activationClasses"`
	ProhibitedInterfaces []string  `json:"prohibitedInterfaces"`
	GitOpsExcludes       []string  `json:"gitopsExcludes"`
}

type Runtime struct {
	Aggregation       string   `json:"aggregation"`
	Isolation         string   `json:"isolation"`
	ProviderPlacement string   `json:"providerPlacement"`
	ConsumerPlacement string   `json:"consumerPlacement"`
	DefaultTransport  string   `json:"defaultTransport"`
	ProviderOrigins   []string `json:"providerOrigins"`
}

type Authority struct {
	TenantControl    string `json:"tenantControl"`
	ProviderProtocol string `json:"providerProtocol"`
	AgentDelivery    string `json:"agentDelivery"`
	DynamicState     string `json:"dynamicState"`
}

type Surface struct {
	ID              string   `json:"id"`
	BindingRequired bool     `json:"bindingRequired"`
	ScopeRequired   bool     `json:"scopeRequired"`
	Operations      []string `json:"operations"`
}

func Parse(data []byte) (Provider, error) {
	var provider Provider
	if err := json.Unmarshal(data, &provider); err != nil {
		return Provider{}, fmt.Errorf("parse provider contract: %w", err)
	}
	if err := provider.Validate(); err != nil {
		return Provider{}, err
	}
	return provider, nil
}

func (p Provider) Validate() error {
	if p.SchemaVersion != "stalwart-agent-tools.re8ch.com/v1alpha1" {
		return fmt.Errorf("unsupported schema version %q", p.SchemaVersion)
	}
	if p.Provider != "stalwart" || p.ServiceClass != "collaboration.stalwart.v1" {
		return fmt.Errorf("provider identity does not match Stalwart service class")
	}
	if p.Runtime.DefaultTransport != "public-edge" {
		return fmt.Errorf("default transport must remain public-edge")
	}
	if p.Authority.ProviderProtocol != "8iops/stalwart-agent-tools" {
		return fmt.Errorf("provider protocol authority must remain in this repository")
	}
	if len(p.Surfaces) != 3 {
		return fmt.Errorf("user, organization, and platform surfaces are required")
	}
	seen := map[string]bool{}
	for _, surface := range p.Surfaces {
		if seen[surface.ID] || len(surface.Operations) == 0 || !surface.ScopeRequired {
			return fmt.Errorf("invalid surface %q", surface.ID)
		}
		seen[surface.ID] = true
	}
	for _, required := range []string{"user", "organization", "platform"} {
		if !seen[required] {
			return fmt.Errorf("missing %s surface", required)
		}
	}
	return nil
}
