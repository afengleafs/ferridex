package server

import (
	"bytes"
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
		"console":                      false,
		"pane-upstream":                false,
		"pane-providers":               false,
		"pane-access":                  false,
		"pane-clients":                 false,
		"pane-logs":                    false,
		"healthbar":                    false,
		"upstream-hub":                 false,
		"claude-pane":                  false,
		"codex-pane":                   false,
		"grok-pane":                    false,
		"cursor-pane":                  false,
		"claude-upstream-sources":      false,
		"claude-upstream-reload":       false,
		"hub-copy-launch":              false,
		"hub-public-toggle":            false,
		"responses-cards":              false,
		"upstream-source-subscription": false,
		"upstream-source-custom":       false,
		"upstream-form":                false,
		"upstream-name":                false,
		"upstream-base-url":            false,
		"upstream-model":               false,
		"upstream-api-key":             false,
		"upstream-save":                false,
		"upstream-test":                false,
		"upstream-activate":            false,
		"upstream-use-subscription":    false,
		"upstream-clear":               false,
		"upstream-error":               false,
		"upstream-test-result":         false,
		"providers":                    false,
		"cards":                        false,
		"tunnel-panel":                 false,
		"public-panel":                 false,
		"endpoints-panel":              false,
		"endpoints":                    false,
		"clients":                      false,
		"target-toggle":                false,
		"target-ssh":                   false,
		"target-public":                false,
		"logs":                         false,
	}
	ids := make(map[string]struct{})
	var visit func(*html.Node)
	visit = func(node *html.Node) {
		if node.Type == html.ElementNode {
			id := htmlAttribute(node, "id")
			if id != "" {
				if _, exists := ids[id]; exists {
					t.Errorf("duplicate dashboard id %q", id)
				}
				ids[id] = struct{}{}
				if _, ok := required[id]; ok {
					required[id] = true
				}
			}
			if id == "upstream-api-key" {
				if got := htmlAttribute(node, "type"); got != "password" {
					t.Errorf("API key input type = %q, want password", got)
				}
				if got := htmlAttribute(node, "autocomplete"); got != "new-password" {
					t.Errorf("API key autocomplete = %q, want new-password", got)
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
}

func htmlAttribute(node *html.Node, name string) string {
	for _, attribute := range node.Attr {
		if attribute.Key == name {
			return attribute.Val
		}
	}
	return ""
}
