package agent

import (
	"testing"

	"agentforge/internal/config"
	"agentforge/internal/provider"
)

func TestDefaultProviderFactory(t *testing.T) {
	cases := []struct {
		provider string
		wantName string
		wantErr  bool
	}{
		{provider: "ollama", wantName: "ollama"},
		{provider: "anthropic", wantName: "anthropic"},
		{provider: "openai", wantErr: true}, // accepted by config validation, not yet implemented
		{provider: "made-up", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			p, err := DefaultProviderFactory(config.ModelConfig{Provider: tc.provider, Name: "some-model", APIKey: "k"})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error for provider %q", tc.provider)
				}
				return
			}
			if err != nil {
				t.Fatalf("DefaultProviderFactory(%q): %v", tc.provider, err)
			}
			if p.Name() != tc.wantName {
				t.Fatalf("Name() = %q, want %q", p.Name(), tc.wantName)
			}
		})
	}
}

func TestDefaultProviderFactoryPassesAPIKeyToAnthropic(t *testing.T) {
	p, err := DefaultProviderFactory(config.ModelConfig{Provider: "anthropic", Name: "claude-sonnet-4-6", APIKey: "sk-ant-test"})
	if err != nil {
		t.Fatalf("DefaultProviderFactory: %v", err)
	}
	a, ok := p.(*provider.Anthropic)
	if !ok {
		t.Fatalf("expected *provider.Anthropic, got %T", p)
	}
	if a.APIKey != "sk-ant-test" {
		t.Fatalf("APIKey = %q, want %q", a.APIKey, "sk-ant-test")
	}
}
