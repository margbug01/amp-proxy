package customproxy

import (
	"net/http/httptest"
	"testing"

	"github.com/margbug01/amp-proxy/internal/config"
)

func TestRegistryProviderLookupIsCaseInsensitive(t *testing.T) {
	upstream := httptest.NewServer(nil)
	defer upstream.Close()

	reg := &Registry{}
	if err := reg.Configure([]config.CustomProvider{
		{
			Name:   "mixed-case",
			URL:    upstream.URL,
			Models: []string{"GPT-5.4-Mini"},
		},
	}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	provider := reg.ProviderForModel("gpt-5.4-mini")
	if provider == nil {
		t.Fatal("ProviderForModel() returned nil")
	}
	if provider.Name != "mixed-case" {
		t.Fatalf("provider.Name = %q, want mixed-case", provider.Name)
	}
	if len(provider.Models) != 1 || provider.Models[0] != "GPT-5.4-Mini" {
		t.Fatalf("provider.Models = %#v, want original mixed-case model", provider.Models)
	}
	if proxy := reg.ProxyForModel("gpt-5.4-mini(HIGH)"); proxy == nil {
		t.Fatal("ProxyForModel() returned nil for case-insensitive suffix lookup")
	}
	models := reg.ModelIDs()
	if len(models) != 1 || models[0] != "GPT-5.4-Mini" {
		t.Fatalf("ModelIDs() = %#v, want original mixed-case model", models)
	}
}
