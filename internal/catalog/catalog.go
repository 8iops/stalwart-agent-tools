package catalog

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type Catalog struct {
	SchemaVersion string      `json:"schemaVersion"`
	Operations    []Operation `json:"operations"`
}

type Operation struct {
	ID              string   `json:"id"`
	Surface         string   `json:"surface"`
	Scope           string   `json:"scope"`
	Mode            string   `json:"mode"`
	ProviderMethods []string `json:"providerMethods"`
	Requires        []string `json:"requires"`
	Outcome         string   `json:"outcome,omitempty"`
}

func Parse(data []byte) (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return Catalog{}, fmt.Errorf("parse tool catalog: %w", err)
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (c Catalog) Validate() error {
	if c.SchemaVersion != "stalwart-agent-tools.re8ch.com/tool-catalog-v1alpha1" {
		return fmt.Errorf("unsupported tool catalog schema %q", c.SchemaVersion)
	}
	if len(c.Operations) == 0 {
		return fmt.Errorf("tool catalog is empty")
	}
	seen := map[string]bool{}
	for _, operation := range c.Operations {
		if err := operation.Validate(); err != nil {
			return fmt.Errorf("operation %q: %w", operation.ID, err)
		}
		if seen[operation.ID] {
			return fmt.Errorf("duplicate operation %q", operation.ID)
		}
		seen[operation.ID] = true
	}
	return nil
}

func (o Operation) Validate() error {
	if o.ID == "" || strings.ContainsAny(o.ID, " /:") {
		return fmt.Errorf("invalid operation id")
	}
	if !slices.Contains([]string{"user", "organization", "platform"}, o.Surface) {
		return fmt.Errorf("invalid surface %q", o.Surface)
	}
	if o.Scope == "" || o.Scope == "stalwart.full" {
		return fmt.Errorf("explicit narrow product scope is required")
	}
	if !slices.Contains([]string{"read", "write"}, o.Mode) {
		return fmt.Errorf("invalid mode %q", o.Mode)
	}
	if len(o.ProviderMethods) == 0 {
		return fmt.Errorf("typed provider methods are required")
	}
	for _, method := range o.ProviderMethods {
		if method == "*" || strings.HasPrefix(method, "http://") || strings.HasPrefix(method, "https://") {
			return fmt.Errorf("arbitrary provider method or target is forbidden")
		}
	}
	if o.Mode == "write" {
		for _, required := range []string{"preview", "expected-state", "idempotency", "read-back"} {
			if !slices.Contains(o.Requires, required) {
				return fmt.Errorf("write is missing %s", required)
			}
		}
		if o.Outcome == "" {
			return fmt.Errorf("write outcome semantics are required")
		}
	}
	return nil
}
