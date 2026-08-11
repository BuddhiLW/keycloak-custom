// Package render turns validated tokens into the two generated artifacts: the
// stylesheet the pod will serve, and the ConfigMap that carries it.
package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/BuddhiLW/keycloak-custom/internal/tokens"
)

// MaxCSS is the per-tenant stylesheet budget. A ConfigMap is hard-capped at
// 1 MiB by the API server, and that cap is shared with every other key in the
// object; 256 KiB leaves an order of magnitude of headroom over today's ~3 KB
// and still fails loudly long before the API server does.
const MaxCSS = 256 * 1024

// View is what the template sees. Derived values are computed here so the
// template stays declarative and a tenant cannot express them.
type View struct {
	*tokens.Theme
	FocusRing      string
	OnAccent       string
	Gradient       string
	LightGradient  string
	LightFocusRing string
	LightOnAccent  string
	LogoDataURI    string
	LogoWidth      int
	LogoHeight     int
	// LogoCSSValue is the complete value of --keycloak-logo-url, url() wrappers
	// included, because the raster case is a two-layer background the template
	// must not have to assemble. For the generated SVG it is exactly
	// url("<LogoDataURI>"), which keeps output byte-identical for every theme
	// written before brand.logo existed.
	LogoCSSValue string
	// LightLogoDataURI is the same lockup painted in the light palette. The logo
	// is a background image, so it cannot inherit currentColor: without a second
	// URI a dark-first tenant serves its dark wordmark onto a near-white card.
	LightLogoDataURI string
}

// logoSVG reproduces the product's own brand lockup — an accent square holding
// a glyph, then the wordmark — as an inline SVG, base64'd into a data: URI.
//
// base64 rather than percent-encoding: an SVG is full of `#`, `<` and `"`, each
// of which has to be escaped differently inside a CSS url(), and getting one
// wrong yields a silently blank logo.
//
// Webfonts do not load inside a data: URI, so the SVG names a generic stack; the
// wordmark renders in the system sans rather than the product's display face.
// That is the documented cost of shipping no binary asset.
//
// accent, onAccent and text are passed rather than read off the theme because
// the same lockup is painted once per colour scheme; geometry is identical, so
// only the fills differ.
func logoSVG(t *tokens.Theme, accent, onAccent, text string) (uri string, w, h int) {
	if t.Brand.Wordmark == "" {
		return "", 0, 0
	}
	w, h = 12*len([]rune(t.Brand.Wordmark))+52, 40
	svg := fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d">`+
			`<rect x="0" y="5" width="30" height="30" rx="9" fill="%s"/>`+
			`<text x="15" y="26" text-anchor="middle" font-family="system-ui,sans-serif" font-size="17" font-weight="800" fill="%s">%s</text>`+
			`<text x="40" y="27" font-family="system-ui,sans-serif" font-size="20" font-weight="800" letter-spacing="-0.8" fill="%s">%s</text>`+
			`</svg>`,
		w, h, w, h,
		accent, onAccent, html.EscapeString(t.Brand.Mark),
		text, html.EscapeString(t.Brand.Wordmark))
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg)), w, h
}

func newView(t *tokens.Theme) (View, error) {
	v := View{
		Theme:     t,
		FocusRing: tokens.RGBA(t.Tokens.Accent, 0.10),
		OnAccent:  t.OnAccent(),
	}
	v.Gradient = gradient(t.Page.Gradient.Kind, t.Page.Gradient.Tint)
	v.LogoDataURI, v.LogoWidth, v.LogoHeight = logoSVG(t, t.Tokens.Accent, t.OnAccent(), t.Tokens.Text)
	if v.LogoDataURI != "" {
		v.LogoCSSValue = fmt.Sprintf("url(%q)", v.LogoDataURI)
	}
	if t.Light != nil {
		v.LightGradient = gradient(t.Page.Gradient.Kind, t.Light.GradientTint)
		v.LightFocusRing = tokens.RGBA(t.Light.Accent, 0.10)
		v.LightOnAccent = t.LightOnAccent()
		v.LightLogoDataURI, _, _ = logoSVG(t, t.Light.Accent, t.LightOnAccent(), t.Light.Text)
	}
	// A raster logo replaces the generated lockup entirely, including the light
	// repaint: an image carries its own colours and has nothing to re-derive.
	if l := t.Brand.Logo; l != nil {
		placeholder, err := t.PlaceholderData()
		if err != nil {
			return View{}, fmt.Errorf("brand.logo.placeholder: %w", err)
		}
		// Layer order is the whole mechanism: CSS paints the FIRST-listed
		// background on top, so the full-resolution file covers the embedded
		// placeholder the moment it arrives, and the placeholder is what the
		// user sees until then. Reversing these two lines silently produces a
		// page that never shows the real artwork.
		//
		// No src means the placeholder IS the artwork — a vector needs nothing
		// sharpened over it — and the login page fetches no logo at all.
		if l.Src == "" {
			v.LogoCSSValue = fmt.Sprintf("url(%q)", placeholder)
		} else {
			v.LogoCSSValue = fmt.Sprintf("url(%q), url(%q)", l.Src, placeholder)
		}
		v.LogoWidth, v.LogoHeight = l.Width, l.Height
		v.LightLogoDataURI = ""
	}
	return v, nil
}

func gradient(kind, tint string) string {
	switch kind {
	case "radial":
		return fmt.Sprintf("radial-gradient(1200px 600px at 50%% -10%%, %s 0%%, var(--kc-brand-bg) 60%%) fixed", tint)
	case "linear":
		return fmt.Sprintf("linear-gradient(180deg, %s 0%%, var(--kc-brand-bg) 45%%) fixed", tint)
	default:
		// Flat. A product whose own site uses a flat ground should not get a
		// gradient invented for it on the one page that fronts it.
		return "var(--kc-brand-bg)"
	}
}

// CSS renders the stylesheet. templateRoot is the keycloak-custom template dir.
func CSS(t *tokens.Theme, templateRoot string) (string, error) {
	p := filepath.Join(templateRoot, t.Template, "theme.css.tmpl")
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("template %s: %w", t.Template, err)
	}
	// Option("missingkey=error"): a template referencing a field that does not
	// exist must fail the build, not emit "<no value>" into a stylesheet.
	tpl, err := template.New("theme.css").Option("missingkey=error").Parse(string(b))
	if err != nil {
		return "", err
	}
	v, err := newView(t)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, v); err != nil {
		return "", err
	}
	s := out.String()
	if len(s) > MaxCSS {
		return "", fmt.Errorf("generated CSS is %d bytes, over the %d byte budget", len(s), MaxCSS)
	}
	return s, nil
}

// Build is the provenance record shipped alongside the CSS. The verifier reads
// its sha to tell "this tenant's CSS is live" from "this tenant fell back to
// stock", which is otherwise silent.
type Build struct {
	Tenant   string `json:"tenant"`
	Scheme   string `json:"scheme"`
	Template string `json:"template"`
	CSSSha   string `json:"css_sha256"`
	Rendered string `json:"rendered_at,omitempty"`
}

func BuildJSON(t *tokens.Theme, css string, stamp time.Time) (string, Build, error) {
	b := Build{
		Tenant:   t.Tenant,
		Scheme:   string(t.Scheme),
		Template: t.Template,
		CSSSha:   fmt.Sprintf("%x", sha256.Sum256([]byte(css))),
	}
	if !stamp.IsZero() {
		b.Rendered = stamp.UTC().Format(time.RFC3339)
	}
	j, err := json.MarshalIndent(b, "", "  ")
	return string(j) + "\n", b, err
}

// ConfigMap emits the tenant's ConfigMap. Only `theme.css` and `build.json` are
// ever projected by the shared Keycloak CR, so any other key a tenant adds here
// is inert — kubelet writes only the keys named in the CR's items[].
//
// suffix is "" for production and "-staging" for staging: both Keycloak
// instances share namespace `keycloak`, so the names must differ.
func ConfigMap(t *tokens.Theme, css, buildJSON, suffix string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, `# GENERATED by kctheme render — do not edit. CI runs "kctheme render --check".
#
# Only the keys named in the shared Keycloak CR's items[] are projected into the
# pod (theme.css, build.json). Adding another key here has no effect on the
# server; it cannot reach login/theme.properties, which is platform-owned.
apiVersion: v1
kind: ConfigMap
metadata:
  name: kc-theme-%s%s
  namespace: keycloak
  labels:
    app.kubernetes.io/component: keycloak-theme
    keycloak.theme/tenant: %s
data:
  theme.css: |
`, t.Tenant, suffix, t.Tenant)
	for _, line := range strings.Split(strings.TrimRight(css, "\n"), "\n") {
		if line == "" {
			sb.WriteString("\n")
			continue
		}
		sb.WriteString("    " + line + "\n")
	}
	sb.WriteString("  build.json: |\n")
	for _, line := range strings.Split(strings.TrimRight(buildJSON, "\n"), "\n") {
		sb.WriteString("    " + line + "\n")
	}
	return sb.String()
}

// Registration prints the exact hunk that must be added to the shared Keycloak
// CR to give this tenant a mount. This is the one irreducible cross-repo step,
// so the tool emits it verbatim rather than describing it.
func Registration(t *tokens.Theme, suffix string) string {
	return fmt.Sprintf(`        # --- tenant: %s -------------------------------------------------
        # volumes[] entry
        - name: theme-%s
          projected:
            defaultMode: 0444
            sources:
              # PLATFORM-owned. Mandatory: a theme dir whose theme.properties is
              # absent yields HTTP 500 (FreeMarkerException) while still
              # enumerating as a selectable theme.
              - configMap:
                  name: kc-theme-properties
                  optional: false
                  items:
                    - key: %s
                      path: login/theme.properties
              # TENANT-owned. Optional: if this ConfigMap or its keys are absent
              # the login page still renders 200 with parent styling and the
              # brand sheet 404s. Verified on 26.6.1.
              - configMap:
                  name: kc-theme-%s%s
                  optional: true
                  items:
                    - key: theme.css
                      path: login/resources/css/theme.css
                    - key: build.json
                      path: login/resources/build.json

        # containers[0] volumeMounts[] entry — containers[0] MUST remain the
        # server container; the operator renames whatever sits at index 0 to
        # "keycloak" and assigns it spec.image.
        - name: theme-%s
          mountPath: /opt/keycloak/themes/%s
          readOnly: true
`, t.Tenant, t.Tenant, t.Scheme.PlatformKey(), t.Tenant, suffix, t.Tenant, t.Tenant)
}
