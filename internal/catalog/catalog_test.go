package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishedCatalogIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "contracts", "tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(data); err != nil {
		t.Fatal(err)
	}
}

func TestWriteNeedsSafetyContract(t *testing.T) {
	operation := Operation{
		ID:              "mail.send",
		Surface:         "user",
		Scope:           "mail.send",
		Mode:            "write",
		ProviderMethods: []string{"EmailSubmission/set"},
		Requires:        []string{"preview"},
		Outcome:         "provider-submitted",
	}
	if err := operation.Validate(); err == nil {
		t.Fatal("unsafe write contract was accepted")
	}
}

func TestArbitraryProviderMethodIsRejected(t *testing.T) {
	operation := Operation{
		ID:              "provider.call",
		Surface:         "platform",
		Scope:           "stalwart.platform.configuration",
		Mode:            "read",
		ProviderMethods: []string{"*"},
	}
	if err := operation.Validate(); err == nil {
		t.Fatal("arbitrary provider method was accepted")
	}
}
