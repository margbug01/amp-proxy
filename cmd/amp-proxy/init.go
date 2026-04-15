package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// runInit is the entry point for the `amp-proxy init` subcommand. It prompts
// the operator for the handful of values that cannot be defaulted (custom
// provider URL, custom provider Bearer token, optional ampcode.com upstream
// key, Gemini route mode), generates a random local API key, and writes a
// ready-to-run config.yaml to the requested path. Host/port, the full Amp
// CLI model mapping table, and sensible defaults for everything else are
// baked in so the produced file runs unmodified against a standard Amp CLI
// setup.
//
// Anything that would need non-trivial customisation (multiple providers,
// per-client upstream keys, access manager, body capture) is intentionally
// left out of the generated file; operators who need those can hand-edit
// afterwards or copy from config.example.yaml.
func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", "config.yaml", "path to write the generated config file")
	force := fs.Bool("force", false, "overwrite the target file if it already exists")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if _, err := os.Stat(*configPath); err == nil && !*force {
		return fmt.Errorf("refusing to overwrite existing %s — delete it, pass -force, or use -config <other-path>", *configPath)
	}

	fmt.Println("amp-proxy init — answer a few questions and a ready-to-run config will be written.")
	fmt.Println("Values are echoed to the terminal; clear your shell history if the API key is sensitive.")
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	gatewayURL, err := promptRequired(reader, "Custom provider URL (OpenAI-compatible, e.g. http://host:port/v1)", "")
	if err != nil {
		return err
	}
	gatewayKey, err := promptRequired(reader, "Custom provider API key (Bearer token)", "")
	if err != nil {
		return err
	}
	geminiMode, err := promptChoice(reader, "Gemini route mode", []string{"translate", "ampcode"}, "translate")
	if err != nil {
		return err
	}
	ampUpstream, err := promptOptional(reader, "Amp upstream API key (for ampcode.com fallback, press Enter to skip)", "")
	if err != nil {
		return err
	}

	localKey, err := generateLocalAPIKey()
	if err != nil {
		return fmt.Errorf("generate local API key: %w", err)
	}

	content := renderInitConfig(gatewayURL, gatewayKey, ampUpstream, geminiMode, localKey)
	if err := os.WriteFile(*configPath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", *configPath, err)
	}

	fmt.Println()
	fmt.Printf("Wrote %s (mode 600).\n", *configPath)
	fmt.Println()
	fmt.Println("Start amp-proxy:")
	fmt.Printf("  ./amp-proxy --config %s\n", *configPath)
	fmt.Println()
	fmt.Println("Point Amp CLI at it:")
	fmt.Println("  export AMP_URL=http://127.0.0.1:8317")
	fmt.Printf("  export AMP_API_KEY=%s\n", localKey)
	fmt.Println("  amp")
	return nil
}

// promptRequired reads a non-empty line from the user. Empty responses
// trigger a re-prompt; EOF returns the default value if set, otherwise an
// error.
func promptRequired(r *bufio.Reader, label, defaultVal string) (string, error) {
	for {
		if defaultVal != "" {
			fmt.Printf("%s [%s]: ", label, defaultVal)
		} else {
			fmt.Printf("%s: ", label)
		}
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && defaultVal != "" {
				return defaultVal, nil
			}
			return "", fmt.Errorf("read %q: %w", label, err)
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if defaultVal != "" {
				return defaultVal, nil
			}
			fmt.Println("  value required, please try again")
			continue
		}
		return trimmed, nil
	}
}

// promptOptional reads a line from the user, returning the default (or
// empty string) on an empty response. Used for fields the operator may
// legitimately want to leave blank, such as the ampcode.com upstream key.
func promptOptional(r *bufio.Reader, label, defaultVal string) (string, error) {
	if defaultVal != "" {
		fmt.Printf("%s [%s]: ", label, defaultVal)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := r.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			return defaultVal, nil
		}
		return "", fmt.Errorf("read %q: %w", label, err)
	}
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return defaultVal, nil
	}
	return trimmed, nil
}

// promptChoice reads a line from the user and restricts the response to
// one of the provided choices. A case-insensitive empty response returns
// the default.
func promptChoice(r *bufio.Reader, label string, choices []string, defaultVal string) (string, error) {
	lowered := make([]string, len(choices))
	for i, c := range choices {
		lowered[i] = strings.ToLower(c)
	}
	for {
		fmt.Printf("%s (%s) [%s]: ", label, strings.Join(choices, "/"), defaultVal)
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return defaultVal, nil
			}
			return "", fmt.Errorf("read %q: %w", label, err)
		}
		trimmed := strings.TrimSpace(strings.ToLower(line))
		if trimmed == "" {
			return defaultVal, nil
		}
		for _, c := range lowered {
			if trimmed == c {
				return trimmed, nil
			}
		}
		fmt.Printf("  invalid choice %q, must be one of %v\n", trimmed, choices)
	}
}

// generateLocalAPIKey returns a URL-safe hex token that amp-proxy will
// require on incoming Amp CLI requests. 16 random bytes (32 hex chars) is
// large enough that a local attacker cannot brute-force it in any
// meaningful time on a loopback-bound server.
func generateLocalAPIKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "amp-" + hex.EncodeToString(b), nil
}

// renderInitConfig produces a complete config.yaml body from the prompted
// values. Layout and comments mirror config.example.yaml so operators who
// later want to cross-reference the example can find their way around.
func renderInitConfig(gatewayURL, gatewayKey, ampUpstream, geminiMode, localKey string) string {
	var b strings.Builder
	b.WriteString("# Generated by `amp-proxy init`.\n")
	b.WriteString("# Edit freely — amp-proxy hot-reloads most fields without restart.\n")
	b.WriteString("\n")
	b.WriteString("host: \"127.0.0.1\"\n")
	b.WriteString("port: 8317\n")
	b.WriteString("\n")
	b.WriteString("# Local API keys Amp CLI must present (match AMP_API_KEY in your shell).\n")
	b.WriteString("api-keys:\n")
	fmt.Fprintf(&b, "  - %q\n", localKey)
	b.WriteString("\n")
	b.WriteString("ampcode:\n")
	b.WriteString("  upstream-url: \"https://ampcode.com\"\n")
	fmt.Fprintf(&b, "  upstream-api-key: %q\n", ampUpstream)
	b.WriteString("  restrict-management-to-localhost: true\n")
	b.WriteString("\n")
	b.WriteString("  # Rewrite Amp CLI model names onto the gpt-5.4 family served by\n")
	b.WriteString("  # custom-providers below. Adjust the right-hand side if your gateway\n")
	b.WriteString("  # exposes different model names.\n")
	b.WriteString("  model-mappings:\n")
	mappings := [][2]string{
		{"claude-opus-4-6", "gpt-5.4(high)"},
		{"claude-sonnet-4-6-thinking", "gpt-5.4-mini(high)"},
		{"claude-haiku-4-5-20251001", "gpt-5.4-mini"},
		{"gpt-5.4", "gpt-5.4(xhigh)"},
		{"gemini-2.5-flash-lite-preview-09-2025", "gpt-5.4-mini"},
		{"gemini-2.5-flash-lite", "gpt-5.4-mini"},
		{"claude-sonnet-4-6", "gpt-5.4-mini(high)"},
		{"gpt-5.3-codex", "gpt-5.4(high)"},
		{"gemini-3-flash-preview", "gpt-5.4-mini(high)"},
	}
	for _, m := range mappings {
		fmt.Fprintf(&b, "    - from: %q\n", m[0])
		fmt.Fprintf(&b, "      to: %q\n", m[1])
	}
	b.WriteString("\n")
	b.WriteString("  force-model-mappings: true\n")
	b.WriteString("\n")
	b.WriteString("  custom-providers:\n")
	b.WriteString("    - name: \"gateway\"\n")
	fmt.Fprintf(&b, "      url: %q\n", gatewayURL)
	fmt.Fprintf(&b, "      api-key: %q\n", gatewayKey)
	b.WriteString("      models:\n")
	b.WriteString("        - \"gpt-5.4\"\n")
	b.WriteString("        - \"gpt-5.4-mini\"\n")
	b.WriteString("\n")
	fmt.Fprintf(&b, "  gemini-route-mode: %q\n", geminiMode)
	return b.String()
}
