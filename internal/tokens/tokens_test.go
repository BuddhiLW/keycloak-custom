package tokens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// baseYAML is a minimal theme that must validate clean. Every rejection case
// below is this document plus one clause, so a failure names the clause.
const baseYAML = `tenant: acme
scheme: light-only
template: v1
tokens:
  bg: "#f5f7fa"
  surface: "#ffffff"
  border: "#dde3ec"
  text: "#161b24"
  muted: "#5d6b80"
  accent: "#2f6feb"
  accent_hover: "#2458c4"
  input_bg: "#ffffff"
`

// darkFirstYAML is the other half of the scheme space: it carries the light
// override block, which has its own required-field set.
const darkFirstYAML = `tenant: acme
scheme: dark-first
template: v1
tokens:
  bg: "#0b1020"
  surface: "#141b31"
  border: "#26304d"
  text: "#eef1f8"
  muted: "#9aa6c4"
  accent: "#5b8cff"
  accent_hover: "#4577f0"
  input_bg: "#0e1426"
light:
  bg: "#f4f6fb"
  surface: "#ffffff"
  border: "#dbe1ee"
  text: "#111629"
  muted: "#5b6580"
  input_bg: "#fbfcff"
  accent: "#2a5bd7"
  accent_hover: "#1f47ad"
`

// loadYAML writes doc to a temp file and loads it, so the test exercises the
// real decoder path (KnownFields, BaseDir) rather than a hand-built struct.
func loadYAML(t *testing.T, doc string) (*Theme, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "theme.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func mustLoad(t *testing.T, doc string) *Theme {
	t.Helper()
	th, err := loadYAML(t, doc)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return th
}

func TestBaseThemesValidate(t *testing.T) {
	for name, doc := range map[string]string{
		"light-only": baseYAML,
		"dark-first": darkFirstYAML,
		"dark-only":  strings.Replace(baseYAML, "scheme: light-only", "scheme: dark-only", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if errs := mustLoad(t, doc).Validate(); len(errs) > 0 {
				t.Fatalf("expected no errors, got %v", errs)
			}
		})
	}
}

// TestRejects covers the tenant-facing contract. Everything here is something
// that would otherwise reach generated CSS, so a case dropping out of this
// table is a hole in the boundary, not a missing nicety.
func TestRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		// --- CSS escape attempts: the residual surface a tenant still supplies.
		{
			"shadow closes its declaration",
			baseYAML + "card:\n  shadow: \"0 1px 2px #000; } body { display: none\"\n",
			"card.shadow",
		},
		{
			"shadow smuggles an import",
			baseYAML + "card:\n  shadow: \"0 0 0 #000 @import url(evil.css)\"\n",
			"card.shadow",
		},
		{
			"length carries a second declaration",
			baseYAML + "card:\n  radius: \"12px; position: fixed\"\n",
			"card.radius",
		},
		{
			"padding carries a second declaration",
			baseYAML + "control:\n  input_padding: \"13px 14px; color: red\"\n",
			"control.input_padding",
		},
		{
			"font stack escapes the declaration",
			baseYAML + "font:\n  body: \"system-ui; } html {\"\n",
			"font.body",
		},

		// --- the logo URL lands inside url("…"): a quote terminates it.
		{
			"logo src closes the url()",
			baseYAML + "brand:\n  logo:\n    src: \"https://a.example/l.png\\\") ; x: url(\\\"y\"\n    placeholder: \"p.svg\"\n    width: 10\n    height: 10\n",
			"brand.logo.src",
		},
		{
			"logo src is not https",
			baseYAML + "brand:\n  logo:\n    src: \"http://a.example/l.png\"\n    placeholder: \"p.svg\"\n    width: 10\n    height: 10\n",
			"brand.logo.src",
		},

		// --- third-party fetches from the page where credentials are typed.
		{
			"font import from an arbitrary origin",
			baseYAML + "font:\n  import_url: \"https://evil.example/f.css\"\n",
			"font.import_url",
		},
		{
			"font import over http",
			baseYAML + "font:\n  import_url: \"http://fonts.googleapis.com/css2?family=Inter\"\n",
			"font.import_url",
		},

		// --- brand text is interpolated into a generated SVG.
		{
			"wordmark injects markup",
			baseYAML + "brand:\n  mark: \"A\"\n  wordmark: \"</text><script/>\"\n",
			"brand.wordmark",
		},
		{
			"mark is longer than the accent square holds",
			baseYAML + "brand:\n  mark: \"ABC\"\n  wordmark: \"Acme\"\n",
			"brand.mark",
		},

		// --- the tenant name becomes a directory and a ConfigMap suffix.
		{
			"tenant collides with a stock Keycloak theme",
			strings.Replace(baseYAML, "tenant: acme", "tenant: keycloak", 1),
			"reserved",
		},
		{
			"tenant escapes its path segment",
			strings.Replace(baseYAML, "tenant: acme", `tenant: "../evil"`, 1),
			"tenant",
		},
		{
			"tenant is not a DNS label",
			strings.Replace(baseYAML, "tenant: acme", "tenant: Acme_Corp", 1),
			"tenant",
		},

		// --- scheme / light-block coherence.
		{
			"dark-first without a light block",
			strings.Replace(baseYAML, "scheme: light-only", "scheme: dark-first", 1),
			"requires a light: block",
		},
		{
			"light-only carrying a light block",
			baseYAML + "light:\n  bg: \"#ffffff\"\n",
			"must not carry a light: block",
		},
		{
			"unknown scheme",
			strings.Replace(baseYAML, "scheme: light-only", "scheme: solarized", 1),
			"scheme must be one of",
		},

		// --- colours are never defaulted: a missing one must fail loudly rather
		// than silently render someone else's brand.
		{
			"missing colour token",
			strings.Replace(baseYAML, `  accent: "#2f6feb"`+"\n", "", 1),
			"tokens.accent",
		},
		{
			"gradient without a tint",
			baseYAML + "page:\n  gradient:\n    kind: radial\n",
			"page.gradient.tint",
		},

		// --- WCAG AA on the pairs a login page actually reads.
		{
			"body text below 4.5:1",
			strings.Replace(baseYAML, `  text: "#161b24"`, `  text: "#c9cfd8"`, 1),
			"text on surface",
		},
		{
			"link colour below 4.5:1",
			strings.Replace(baseYAML,
				"  accent: \"#2f6feb\"\n  accent_hover: \"#2458c4\"",
				"  accent: \"#7fb0ff\"\n  accent_hover: \"#6a9df0\"", 1),
			"accent on surface",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			th, err := loadYAML(t, c.doc)
			if err != nil {
				// A decode error is also a rejection, as long as it names the
				// offending field.
				if !strings.Contains(err.Error(), c.want) {
					t.Fatalf("load error %v does not mention %q", err, c.want)
				}
				return
			}
			errs := th.Validate()
			if len(errs) == 0 {
				t.Fatalf("accepted; expected an error mentioning %q", c.want)
			}
			var joined []string
			for _, e := range errs {
				joined = append(joined, e.Error())
			}
			if !strings.Contains(strings.Join(joined, "\n"), c.want) {
				t.Fatalf("errors %v do not mention %q", joined, c.want)
			}
		})
	}
}

// A typo'd token that silently renders the default is the class of failure this
// design exists to remove, so an unknown field must not decode.
func TestUnknownFieldIsRejected(t *testing.T) {
	_, err := loadYAML(t, baseYAML+"parent: keycloak.v2\n")
	if err == nil {
		t.Fatal("accepted an unknown top-level field")
	}
	if !strings.Contains(err.Error(), "parent") {
		t.Fatalf("error %v does not name the offending field", err)
	}
}

// theme.properties is projected from a platform ConfigMap keyed by this value,
// so the mapping is a deployment fact, not a cosmetic one.
func TestPlatformKey(t *testing.T) {
	for s, want := range map[Scheme]string{
		DarkOnly:  "dark",
		DarkFirst: "dark",
		LightOnly: "light",
	} {
		if got := s.PlatformKey(); got != want {
			t.Errorf("%s: got %q, want %q", s, got, want)
		}
	}
}

func TestDefaults(t *testing.T) {
	th := mustLoad(t, baseYAML)
	for _, c := range []struct{ name, got, want string }{
		{"surface_alt falls back to surface", th.Tokens.SurfaceAlt, th.Tokens.Surface},
		{"dim falls back to muted", th.Tokens.Dim, th.Tokens.Muted},
		{"gradient kind", th.Page.Gradient.Kind, "none"},
		{"card radius", th.Card.Radius, "14px"},
		{"card padding", th.Card.Padding, "24px"},
		{"card width", th.Card.Width, "440px"},
		{"control radius", th.Control.Radius, "10px"},
		{"control font weight", th.Control.FontWeight, "700"},
		{"heading falls back to body", th.Font.Heading, th.Font.Body},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, c.got, c.want)
		}
	}

	// The light block gets the same two fallbacks as the base palette. Without
	// them `kctheme new -scheme dark-first` scaffolds a theme its own validator
	// rejects.
	l := mustLoad(t, darkFirstYAML).Light
	if l.SurfaceAlt != l.Surface {
		t.Errorf("light.surface_alt: got %q, want %q", l.SurfaceAlt, l.Surface)
	}
	if l.Dim != l.Muted {
		t.Errorf("light.dim: got %q, want %q", l.Dim, l.Muted)
	}
}

// The retired theme hard-coded #fff on an accent scoring 3.16:1 against white,
// so the button label is derived rather than stated — unless the product states
// one deliberately.
func TestOnAccentIsDerived(t *testing.T) {
	// On a light page a mid-blue accent reads best under white.
	if got := mustLoad(t, baseYAML).OnAccent(); got != "#ffffff" {
		t.Errorf("light theme: got %q, want #ffffff", got)
	}

	// On a dark page the same class of accent reads better under the page ink —
	// #5b8cff measures 5.39:1 on the dark surface and 3.16:1 on white.
	dark := mustLoad(t, darkFirstYAML)
	if got := dark.OnAccent(); got != dark.Tokens.Bg {
		t.Errorf("dark theme: got %q, want the page ink %q (white scores %.2f:1)",
			got, dark.Tokens.Bg, Contrast("#ffffff", dark.Tokens.Accent))
	}

	// An explicit accent_ink is honoured verbatim.
	stated := mustLoad(t, baseYAML)
	stated.Tokens.AccentInk = "#101010"
	if got := stated.OnAccent(); got != "#101010" {
		t.Errorf("stated accent_ink ignored: got %q", got)
	}

	if dark.LightOnAccent() == "" {
		t.Error("dark-first theme derived no light button label")
	}
	if (&Theme{}).LightOnAccent() != "" {
		t.Error("a theme with no light block must derive no light button label")
	}
}

func TestContrastRatios(t *testing.T) {
	// The two anchors of the WCAG scale.
	if got := Contrast("#ffffff", "#000000"); got < 20.99 || got > 21.01 {
		t.Errorf("black on white: got %.4f, want 21", got)
	}
	if got := Contrast("#7f7f7f", "#7f7f7f"); got != 1 {
		t.Errorf("a colour against itself: got %.4f, want 1", got)
	}
	// Symmetric by definition — the lighter operand is normalised to the top.
	if Contrast("#2f6feb", "#ffffff") != Contrast("#ffffff", "#2f6feb") {
		t.Error("contrast is not symmetric")
	}
}

func TestRGBA(t *testing.T) {
	if got := RGBA("#2f6feb", 0.10); got != "rgba(47, 111, 235, 0.1)" {
		t.Errorf("got %q", got)
	}
	if got := RGBA("#000000", 1); got != "rgba(0, 0, 0, 1)" {
		t.Errorf("got %q", got)
	}
}

// PlaceholderData embeds bytes into a world-readable stylesheet, so what it
// accepts is a security question, not a convenience one.
func TestPlaceholderData(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), 0, 1, 2, 3)
	webp := append([]byte("RIFF0000WEBP"), 0, 1)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 1}

	cases := []struct {
		name    string
		file    string
		body    []byte
		wantPfx string
		wantErr string
	}{
		{name: "png", file: "l.png", body: png, wantPfx: "data:image/png;base64,"},
		{name: "webp", file: "l.webp", body: webp, wantPfx: "data:image/webp;base64,"},
		{name: "jpeg", file: "l.jpg", body: jpeg, wantPfx: "data:image/jpeg;base64,"},
		{name: "svg", file: "l.svg", body: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), wantPfx: "data:image/svg+xml;base64,"},
		{
			name: "svg behind a declaration and a comment",
			file: "l.svg",
			body: []byte("\ufeff<?xml version=\"1.0\"?>\n<!-- a comment -->\n<svg/>"),
			// The extension is never trusted: content decides.
			wantPfx: "data:image/svg+xml;base64,",
		},
		{
			name:    "a PNG extension over non-image bytes",
			file:    "l.png",
			body:    []byte("GIF89a not really"),
			wantErr: "sniffed from content",
		},
		{
			name:    "over the embedded-layer budget",
			file:    "l.png",
			body:    append(png, make([]byte, MaxPlaceholder)...),
			wantErr: "budget",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.file), c.body, 0o644); err != nil {
				t.Fatal(err)
			}
			th := &Theme{BaseDir: dir, Brand: Brand{Logo: &Logo{Placeholder: c.file, Width: 10, Height: 10}}}
			got, err := th.PlaceholderData()
			switch {
			case c.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("got (%q, %v), want an error mentioning %q", got, err, c.wantErr)
				}
			case err != nil:
				t.Fatalf("unexpected error: %v", err)
			case !strings.HasPrefix(got, c.wantPfx):
				t.Fatalf("got %.40q…, want prefix %q", got, c.wantPfx)
			}
		})
	}

	// The path is resolved against theme.yaml's directory and must stay inside
	// it: otherwise a theme repo embeds any file the renderer can read into a
	// world-readable stylesheet.
	for _, p := range []string{"../../etc/passwd", "/etc/passwd", "assets/../../secret.png"} {
		th := &Theme{BaseDir: t.TempDir(), Brand: Brand{Logo: &Logo{Placeholder: p, Width: 10, Height: 10}}}
		if _, err := th.PlaceholderData(); err == nil {
			t.Errorf("%q escaped the theme repo", p)
		}
	}

	// No logo at all is not an error — it is the common case.
	if got, err := (&Theme{}).PlaceholderData(); err != nil || got != "" {
		t.Errorf("no logo: got (%q, %v)", got, err)
	}
}

func TestIsSVG(t *testing.T) {
	for _, ok := range []string{`<svg/>`, "  \n<svg xmlns=\"\">", "<?xml?><svg>", "<!DOCTYPE svg><svg>"} {
		if !isSVG([]byte(ok)) {
			t.Errorf("rejected an SVG: %q", ok)
		}
	}
	for _, no := range []string{``, `<html><svg/></html>`, `<?xml version="1.0"?>`, `<!-- only a comment -->`, "\x89PNG"} {
		if isSVG([]byte(no)) {
			t.Errorf("accepted a non-SVG: %q", no)
		}
	}
}
