package server

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestDashboardUpstreamHubDOM(t *testing.T) {
	markup, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(bytes.NewReader(markup))
	if err != nil {
		t.Fatalf("parse embedded dashboard: %v", err)
	}

	required := map[string]bool{
		"console":                 false,
		"console-tab-upstream":    false,
		"console-tab-providers":   false,
		"console-tab-access":      false,
		"console-tab-clients":     false,
		"console-tab-logs":        false,
		"pane-upstream":           false,
		"pane-providers":          false,
		"pane-access":             false,
		"pane-clients":            false,
		"pane-logs":               false,
		"healthbar":               false,
		"upstream-hub":            false,
		"claude-pane":             false,
		"hub-tab-claude":          false,
		"codex-pane":              false,
		"hub-tab-codex":           false,
		"grok-pane":               false,
		"hub-tab-grok":            false,
		"cursor-pane":             false,
		"hub-tab-cursor":          false,
		"claude-upstream-sources": false,
		"claude-upstream-reload":  false,
		"codex-upstream-sources":  false,
		"codex-upstream-reload":   false,
		"codex-upstream-error":    false,
		"codex-upstream-hint":     false,
		"hub-copy-launch":         false,
		"hub-public-toggle":       false,
		"providers":               false,
		"cards":                   false,
		"tunnel-panel":            false,
		"public-panel":            false,
		"endpoints-panel":         false,
		"endpoints":               false,
		"clients":                 false,
		"target-toggle":           false,
		"target-ssh":              false,
		"target-public":           false,
		"logs":                    false,
	}
	nodes := make(map[string]*html.Node)
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode {
			id := htmlAttribute(node, "id")
			if id != "" {
				if _, exists := nodes[id]; exists {
					t.Errorf("duplicate dashboard id %q", id)
				}
				nodes[id] = node
				if _, ok := required[id]; ok {
					required[id] = true
				}
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			visit(child)
		}
	}
	visit(doc)
	for id, present := range required {
		if !present {
			t.Errorf("dashboard is missing required Responses control %q", id)
		}
	}

	tabPanels := map[string]string{
		"console-tab-upstream":  "pane-upstream",
		"console-tab-providers": "pane-providers",
		"console-tab-access":    "pane-access",
		"console-tab-clients":   "pane-clients",
		"console-tab-logs":      "pane-logs",
		"hub-tab-claude":        "claude-pane",
		"hub-tab-codex":         "codex-pane",
		"hub-tab-grok":          "grok-pane",
		"hub-tab-cursor":        "cursor-pane",
	}
	for tabID, panelID := range tabPanels {
		tab, tabOK := nodes[tabID]
		panel, panelOK := nodes[panelID]
		if !tabOK || !panelOK {
			continue
		}
		if got := htmlAttribute(tab, "role"); got != "tab" {
			t.Errorf("%s role = %q, want tab", tabID, got)
		}
		if got := htmlAttribute(tab, "aria-controls"); got != panelID {
			t.Errorf("%s aria-controls = %q, want %q", tabID, got, panelID)
		}
		if got := htmlAttribute(panel, "role"); got != "tabpanel" {
			t.Errorf("%s role = %q, want tabpanel", panelID, got)
		}
		if got := htmlAttribute(panel, "aria-labelledby"); got != tabID {
			t.Errorf("%s aria-labelledby = %q, want %q", panelID, got, tabID)
		}
	}
}

func TestDashboardThirdPartyAssetsEmbedded(t *testing.T) {
	for _, path := range []string{
		"web/icons/claude.svg",
		"web/icons/openai.svg",
		"web/icons/grok.svg",
	} {
		asset, err := webFS.ReadFile(path)
		if err != nil {
			t.Errorf("read embedded asset %s: %v", path, err)
			continue
		}
		if !bytes.Contains(asset, []byte("<svg")) {
			t.Errorf("embedded asset %s is not SVG", path)
		}
	}

	notices, err := webFS.ReadFile("web/third-party-notices.txt")
	if err != nil {
		t.Fatalf("read embedded third-party notices: %v", err)
	}
	for _, required := range []string{
		"Copyright (c) 2025 Jason Young",
		"Copyright (c) 2023 LobeHub",
		"5ca9459d50ea4beea6a81bbc509de6ec5b6b09ca",
	} {
		if !strings.Contains(string(notices), required) {
			t.Errorf("third-party notices missing %q", required)
		}
	}
}

func TestDashboardUsesSupplierFilesAndTerminology(t *testing.T) {
	script, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"claude_provider.env", "codex_provider.env", "新增供应商", "[供应商名]"} {
		if !bytes.Contains(script, []byte(required)) {
			t.Errorf("dashboard script missing %q", required)
		}
	}
	if bytes.Contains(script, []byte("新增档案")) || bytes.Contains(script, []byte("[档案名]")) {
		t.Error("dashboard still exposes legacy 档案 terminology")
	}
}

func htmlAttribute(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return attribute.Val
		}
	}
	return ""
}
