// Package tokens is the value layer: it defines what a tenant is allowed to say
// and rejects everything else. A tenant expresses brand tokens; it never
// expresses Keycloak theme wiring.
package tokens

import (
	"encoding/base64"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Scheme is the tenant-facing colour-scheme intent.
//
// It maps onto a PLATFORM theme.properties key (see PlatformKey), which is what
// sets `darkMode=`. dark-only and dark-first share that key and differ only in
// whether the generated CSS carries a prefers-color-scheme: light override —
// a purely stylesheet-level distinction, so switching between those two is a
// content change, while switching to or from light-only is a registration
// change requiring a CR edit and a rolling restart.
type Scheme string

const (
	// DarkOnly: the product has no light mode at all (e.g. a site declaring
	// `color-scheme: dark`). Emitting a light override for such a product
	// produces a login page that does not match the app it fronts.
	DarkOnly  Scheme = "dark-only"
	DarkFirst Scheme = "dark-first"
	LightOnly Scheme = "light-only"
)

// PlatformKey is the key in the platform-owned kc-theme-properties ConfigMap
// that the shared Keycloak CR projects onto login/theme.properties.
func (s Scheme) PlatformKey() string {
	if s == LightOnly {
		return "light"
	}
	return "dark"
}

var reserved = map[string]bool{
	"base": true, "keycloak": true, "keycloak.v2": true,
	"common": true, "admin": true, "account": true, "email": true, "login": true,
}

var (
	tenantRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	hexRe    = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	// CSS length: digits with an optional unit. Deliberately narrow — these
	// values are interpolated into generated CSS, so anything that could close a
	// declaration and open a new one must not parse.
	lenRe = regexp.MustCompile(`^[0-9]+(px|rem|em|%)?$`)
	// A length PAIR, for padding shorthand like "13px 14px".
	padRe = regexp.MustCompile(`^[0-9]+(px|rem|em|%)?( [0-9]+(px|rem|em|%)?)*$`)
	// Restricted to the characters a shadow legitimately needs; notably no `;`,
	// `{`, `}`, `:`, `/*`, `@` or `\`, so it cannot escape its declaration or
	// smuggle an @import.
	shadowRe = regexp.MustCompile(`^[0-9a-zA-Z .,()#%-]+$`)
	// A font stack: quoted family names, generics, commas, spaces, hyphens.
	fontRe   = regexp.MustCompile(`^[0-9a-zA-Z ,"'-]+$`)
	weightRe = regexp.MustCompile(`^[1-9]00$`)
	// Brand text is embedded in a generated SVG. Restricted to plain word
	// characters so it cannot inject markup into that SVG or the stylesheet.
	// The accented ranges are Latin-1 letters minus × and ÷ — a product named
	// "Funerária Francana" must be spellable, and a letter carries no markup
	// meaning. Everything structural (<, >, &, ", /) stays out.
	brandRe = regexp.MustCompile(`^[0-9A-Za-zÀ-ÖØ-öø-ÿ .·-]{1,24}$`)
	// Only Google Fonts is allowed as a remote stylesheet, and only over https.
	// A login page fetching an arbitrary third-party stylesheet is an injection
	// surface on the one page where credentials are typed.
	fontURLRe = regexp.MustCompile(`^https://fonts\.googleapis\.com/css2\?[A-Za-z0-9=&;:,+@._-]+$`)
	// brand.logo.src lands inside url("…") in a generated declaration, so the
	// character set is deliberately narrower than a URL's. A quote closes the
	// url() and a following ; opens a new declaration — the same class of escape
	// the parent= lockdown exists to prevent, one layer down. Excluding " ' ( )
	// \ ; and whitespace leaves nothing that can terminate the context.
	//
	// https only: this is the page where credentials are typed, and a mixed
	// -content logo is both a downgrade and a silently blank image.
	logoURLRe = regexp.MustCompile(`^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~!$&*+,=:@%/-]*)?$`)
)

type Palette struct {
	Bg          string `yaml:"bg"`
	Surface     string `yaml:"surface"`
	SurfaceAlt  string `yaml:"surface_alt"`
	Border      string `yaml:"border"`
	Text        string `yaml:"text"`
	Muted       string `yaml:"muted"`
	Dim         string `yaml:"dim"`
	Accent      string `yaml:"accent"`
	AccentHover string `yaml:"accent_hover"`
	// AccentInk is the label colour ON the accent. Optional: when empty it is
	// DERIVED as white-or-page-ink, whichever reads better. A product with a
	// deliberate on-accent colour (vtranslate's --mint-ink) can state it, and it
	// is contrast-checked either way.
	AccentInk string `yaml:"accent_ink"`
	InputBg   string `yaml:"input_bg"`
}

type Gradient struct {
	Kind string `yaml:"kind"` // none | radial | linear
	Tint string `yaml:"tint"`
}

type Card struct {
	Radius  string `yaml:"radius"`
	Padding string `yaml:"padding"`
	Shadow  string `yaml:"shadow"`
	Width   string `yaml:"width"`
}

// Control covers the form chrome the theme is allowed to touch: the primary
// button and text inputs. It stops there deliberately — error banners, alerts
// and the OTP/recovery templates shift between Keycloak minors.
type Control struct {
	Radius      string `yaml:"radius"`
	InputRadius string `yaml:"input_radius"`
	InputPad    string `yaml:"input_padding"`
	ButtonPad   string `yaml:"button_padding"`
	FontWeight  string `yaml:"font_weight"`
}

// Brand replaces keycloak.v2's own KEYCLOAK wordmark. Mark and Wordmark are
// plain text: that lockup is GENERATED as an inline SVG and inlined into the
// stylesheet as a data: URI, so a tenant ships no binary and needs no extra
// projected key (the shared CR's items[] are fixed at theme.css + build.json).
//
// Logo overrides that generated lockup with the product's real artwork. It is
// still not an extra projected key: the placeholder is embedded in the
// stylesheet and the full-resolution file is fetched by the browser.
type Brand struct {
	// Mark is the glyph inside the accent square — 1-2 characters.
	Mark string `yaml:"mark"`
	// Wordmark is the product name set beside it. Empty leaves keycloak.v2's
	// stock logo in place.
	Wordmark string `yaml:"wordmark"`
	// Logo is optional. Absent, the generated SVG lockup is used and output is
	// byte-identical to a theme written before this field existed.
	Logo *Logo `yaml:"logo,omitempty"`
}

// Logo is the product's real artwork in place of the generated lockup, in one
// of two shapes.
//
// Two layers, for a raster: a tiny placeholder embedded in the stylesheet,
// painted under a full-resolution file the browser fetches. The layering is
// what makes that cheap. theme.css is render-blocking, so embedding the
// full-resolution artwork would delay the very paint it is meant to improve —
// measured on funerária's own assets, logo-256.webp is 20 KB (~28 KB base64)
// against a ~7 KB stylesheet. Placeholder first, real file second, and CSS
// paints the first-listed background on top: the browser shows the placeholder
// immediately and the real artwork covers it once it arrives, with no script
// and no flash of empty space.
//
// One layer, for a vector: an SVG under MaxPlaceholder is already the final
// artwork at every resolution, so there is nothing for a second layer to
// sharpen. Omitting Src then costs the login page a DNS lookup, a TLS
// handshake and a third origin it does not otherwise need.
type Logo struct {
	// Src is the full-resolution image, fetched by the browser. https only —
	// see logoURLRe for why the character set is narrower than a URL's.
	//
	// Optional. Empty makes Placeholder the only layer, which is right when
	// Placeholder is an SVG and wrong when it is a raster: a raster
	// placeholder is sized for first paint, not for a 2x display.
	Src string `yaml:"src"`
	// Placeholder is a path, relative to theme.yaml, to a small image embedded
	// as a data: URI. Keep it genuinely small; MaxPlaceholder is enforced.
	Placeholder string `yaml:"placeholder"`
	// Width and Height are the painted box in CSS pixels. Required: a raster
	// has no intrinsic size the stylesheet can consult, and guessing here
	// yields a logo that reflows the card when the real file lands.
	Width  int `yaml:"width"`
	Height int `yaml:"height"`
}

type Font struct {
	Body    string `yaml:"body"`
	Heading string `yaml:"heading"`
	// ImportURL is optional and restricted to fonts.googleapis.com. Leaving it
	// empty keeps the login page free of third-party requests; set it only if
	// the product's own site already loads the same families.
	ImportURL string `yaml:"import_url"`
}

// Light is the prefers-color-scheme override, valid only for scheme: dark-first.
//
// Accent and AccentHover are separate from the base tokens because ONE accent
// cannot serve both modes accessibly: measured, #5b8cff scores 5.39:1 on a dark
// surface and 3.16:1 on white, and every colour that fixes white breaks dark.
type Light struct {
	Bg           string `yaml:"bg"`
	Surface      string `yaml:"surface"`
	SurfaceAlt   string `yaml:"surface_alt"`
	Border       string `yaml:"border"`
	Text         string `yaml:"text"`
	Muted        string `yaml:"muted"`
	Dim          string `yaml:"dim"`
	InputBg      string `yaml:"input_bg"`
	GradientTint string `yaml:"gradient_tint"`
	Accent       string `yaml:"accent"`
	AccentHover  string `yaml:"accent_hover"`
	AccentInk    string `yaml:"accent_ink"`
}

type Theme struct {
	Tenant   string  `yaml:"tenant"`
	Scheme   Scheme  `yaml:"scheme"`
	Template string  `yaml:"template"`
	Tokens   Palette `yaml:"tokens"`
	Page     struct {
		Gradient Gradient `yaml:"gradient"`
	} `yaml:"page"`
	Card    Card    `yaml:"card"`
	Control Control `yaml:"control"`
	Font    Font    `yaml:"font"`
	Brand   Brand   `yaml:"brand"`
	Light   *Light  `yaml:"light,omitempty"`

	// BaseDir is theme.yaml's own directory, used to resolve
	// brand.logo.placeholder. It is not a token: KnownFields(true) would reject
	// it in YAML, and a tenant must not be able to point the resolver elsewhere.
	BaseDir string `yaml:"-"`
}

func Load(path string) (*Theme, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Theme
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	// Unknown fields are an error, not a silent no-op: a typo'd token that
	// silently renders the default is exactly the class of failure this design
	// exists to remove.
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	t.BaseDir = filepath.Dir(path)
	t.applyDefaults()
	return &t, nil
}

// MaxPlaceholder caps the embedded first-paint layer. theme.css is
// render-blocking, so this budget IS the two-layer design: past a few KB the
// placeholder costs more paint latency than it saves, and the tenant should
// simply have made a smaller one.
const MaxPlaceholder = 8 * 1024

// PlaceholderData reads brand.logo.placeholder and returns it as a data: URI.
// It returns "" with no error when the theme ships no raster logo.
func (t *Theme) PlaceholderData() (string, error) {
	l := t.Brand.Logo
	if l == nil || l.Placeholder == "" {
		return "", nil
	}
	// The path is resolved against theme.yaml's directory and must stay inside
	// it. Absolute paths and .. would let a theme repo embed any file the
	// renderer can read into a world-readable stylesheet.
	if filepath.IsAbs(l.Placeholder) || strings.Contains(l.Placeholder, "..") {
		return "", fmt.Errorf("must be a relative path inside the theme repo, got %q", l.Placeholder)
	}
	b, err := os.ReadFile(filepath.Join(t.BaseDir, l.Placeholder))
	if err != nil {
		return "", err
	}
	if len(b) > MaxPlaceholder {
		return "", fmt.Errorf("%d bytes exceeds the %d-byte budget (theme.css is render-blocking, so a large placeholder delays the paint it exists to improve)", len(b), MaxPlaceholder)
	}
	// Sniffed, not trusted from the extension: the bytes are what gets embedded,
	// and a data: URI whose MIME disagrees with its payload renders as nothing.
	switch {
	case len(b) > 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b), nil
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "data:image/webp;base64," + base64.StdEncoding.EncodeToString(b), nil
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(b), nil
	case isSVG(b):
		// No magic bytes to sniff, so the root element is the test. Embedding
		// XML is not a wider surface than the three rasters above: base64
		// leaves nothing that can terminate the url("…") it lands in, and a
		// referenced SVG is a document with no scripting and no external
		// fetches — the same restriction the GENERATED lockup already relies on.
		return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(b), nil
	}
	return "", fmt.Errorf("must be a PNG, WebP, JPEG or SVG image (sniffed from content, not the extension)")
}

// isSVG reports whether b is an SVG document: an <svg> root, optionally behind
// a BOM, an XML declaration, a doctype or comments. It scans only the head of
// the file — MaxPlaceholder already bounds the whole of it.
func isSVG(b []byte) bool {
	s := strings.TrimPrefix(string(b), "\ufeff")
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "<svg"):
			return true
		case strings.HasPrefix(s, "<?"), strings.HasPrefix(s, "<!"):
			// An XML declaration, a doctype or a comment: skip it and look at
			// the next node. A file that is only preamble falls out below.
			i := strings.IndexByte(s, '>')
			if i < 0 {
				return false
			}
			s = s[i+1:]
		default:
			return false
		}
	}
}

// applyDefaults fills the layout and typography tokens a tenant may omit. Colour
// tokens deliberately have NO defaults — a missing colour must fail loudly
// rather than silently render someone else's brand.
func (t *Theme) applyDefaults() {
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&t.Page.Gradient.Kind, "none")
	def(&t.Card.Radius, "14px")
	def(&t.Card.Padding, "24px")
	def(&t.Card.Shadow, "0 24px 80px rgba(0, 0, 0, 0.28)")
	def(&t.Card.Width, "440px")
	def(&t.Control.Radius, "10px")
	def(&t.Control.InputRadius, "11px")
	def(&t.Control.InputPad, "13px 14px")
	def(&t.Control.ButtonPad, "11px 16px")
	def(&t.Control.FontWeight, "700")
	def(&t.Font.Body, `system-ui, -apple-system, "Segoe UI", sans-serif`)
	def(&t.Font.Heading, t.Font.Body)
	def(&t.Tokens.SurfaceAlt, t.Tokens.Surface)
	def(&t.Tokens.Dim, t.Tokens.Muted)
}

// Validate is the whole tenant-facing contract. Everything it rejects is
// something that would otherwise reach generated CSS.
func (t *Theme) Validate() []error {
	var errs []error
	bad := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	if !tenantRe.MatchString(t.Tenant) {
		bad("tenant %q must match %s (it becomes a directory name and a ConfigMap suffix)", t.Tenant, tenantRe)
	}
	if reserved[t.Tenant] {
		bad("tenant %q is a reserved Keycloak theme name", t.Tenant)
	}
	switch t.Scheme {
	case DarkOnly, DarkFirst, LightOnly:
	default:
		bad("scheme must be one of %q, %q, %q — got %q", DarkOnly, DarkFirst, LightOnly, t.Scheme)
	}
	if t.Template == "" {
		bad("template is required (e.g. v1)")
	}

	req := map[string]string{
		"tokens.bg": t.Tokens.Bg, "tokens.surface": t.Tokens.Surface,
		"tokens.surface_alt": t.Tokens.SurfaceAlt, "tokens.border": t.Tokens.Border,
		"tokens.text": t.Tokens.Text, "tokens.muted": t.Tokens.Muted,
		"tokens.dim": t.Tokens.Dim, "tokens.accent": t.Tokens.Accent,
		"tokens.accent_hover": t.Tokens.AccentHover, "tokens.input_bg": t.Tokens.InputBg,
	}
	if t.Page.Gradient.Kind != "none" {
		req["page.gradient.tint"] = t.Page.Gradient.Tint
	}
	if t.Tokens.AccentInk != "" {
		req["tokens.accent_ink"] = t.Tokens.AccentInk
	}
	for name, v := range req {
		if !hexRe.MatchString(v) {
			bad("%s must be a 6-digit hex colour like #1a2b3c, got %q", name, v)
		}
	}

	switch t.Page.Gradient.Kind {
	case "none", "radial", "linear":
	default:
		bad(`page.gradient.kind must be "none", "radial" or "linear", got %q`, t.Page.Gradient.Kind)
	}

	for name, v := range map[string]string{
		"card.radius": t.Card.Radius, "card.width": t.Card.Width,
		"control.radius": t.Control.Radius, "control.input_radius": t.Control.InputRadius,
	} {
		if !lenRe.MatchString(v) {
			bad("%s must be a CSS length like 14px, got %q", name, v)
		}
	}
	for name, v := range map[string]string{
		"card.padding": t.Card.Padding, "control.input_padding": t.Control.InputPad,
		"control.button_padding": t.Control.ButtonPad,
	} {
		if !padRe.MatchString(v) {
			bad("%s must be one or more CSS lengths like \"13px 14px\", got %q", name, v)
		}
	}
	if !shadowRe.MatchString(t.Card.Shadow) {
		bad("card.shadow contains characters not allowed in a generated declaration: %q", t.Card.Shadow)
	}
	if !weightRe.MatchString(t.Control.FontWeight) {
		bad("control.font_weight must be a CSS weight like 700, got %q", t.Control.FontWeight)
	}
	for name, v := range map[string]string{"font.body": t.Font.Body, "font.heading": t.Font.Heading} {
		if !fontRe.MatchString(v) {
			bad("%s must be a plain font stack, got %q", name, v)
		}
	}
	if t.Brand.Wordmark != "" {
		if !brandRe.MatchString(t.Brand.Wordmark) {
			bad("brand.wordmark must be plain text (it is embedded in a generated SVG), got %q", t.Brand.Wordmark)
		}
		if !brandRe.MatchString(t.Brand.Mark) || len([]rune(t.Brand.Mark)) > 2 {
			bad("brand.mark must be 1-2 plain characters, got %q", t.Brand.Mark)
		}
	}
	if u := t.Font.ImportURL; u != "" && !fontURLRe.MatchString(u) {
		bad("font.import_url must be an https://fonts.googleapis.com/css2 URL (a login page must not fetch arbitrary third-party stylesheets), got %q", u)
	}
	if l := t.Brand.Logo; l != nil {
		if l.Src != "" && !logoURLRe.MatchString(l.Src) {
			bad("brand.logo.src must be an https:// URL free of quotes, parentheses, backslashes and whitespace (it is written inside url(\"…\") in a generated declaration), got %q", l.Src)
		}
		if l.Width <= 0 || l.Height <= 0 {
			bad("brand.logo.width and brand.logo.height are required and must be positive: a raster has no intrinsic size the stylesheet can consult, and omitting them reflows the card when the full-resolution file lands")
		}
		if l.Placeholder == "" {
			bad("brand.logo.placeholder is required: without an embedded first-paint layer the logo area is empty until the network answers, which is worse than the generated lockup it replaces")
		} else if _, err := t.PlaceholderData(); err != nil {
			bad("brand.logo.placeholder: %v", err)
		}
	}

	switch t.Scheme {
	case DarkFirst:
		if t.Light == nil {
			bad("scheme dark-first requires a light: block (the prefers-color-scheme override); use dark-only if the product has no light mode")
		} else {
			lreq := map[string]string{
				"light.bg": t.Light.Bg, "light.surface": t.Light.Surface,
				"light.surface_alt": t.Light.SurfaceAlt, "light.border": t.Light.Border,
				"light.text": t.Light.Text, "light.muted": t.Light.Muted,
				"light.dim": t.Light.Dim, "light.input_bg": t.Light.InputBg,
				"light.accent": t.Light.Accent, "light.accent_hover": t.Light.AccentHover,
			}
			if t.Page.Gradient.Kind != "none" {
				lreq["light.gradient_tint"] = t.Light.GradientTint
			}
			if t.Light.AccentInk != "" {
				lreq["light.accent_ink"] = t.Light.AccentInk
			}
			for name, v := range lreq {
				if !hexRe.MatchString(v) {
					bad("%s must be a 6-digit hex colour, got %q", name, v)
				}
			}
		}
	case DarkOnly, LightOnly:
		if t.Light != nil {
			bad("scheme %s must not carry a light: block — it would never be emitted", t.Scheme)
		}
	}

	errs = append(errs, t.contrast()...)
	return errs
}

// contrast enforces WCAG AA (4.5:1) on the pairs a login page actually reads.
// A brand palette that fails here produces a page some people cannot use, and
// nothing downstream would catch it.
func (t *Theme) contrast() []error {
	var errs []error
	check := func(label, fg, bg string, min float64) {
		if !hexRe.MatchString(fg) || !hexRe.MatchString(bg) {
			return // already reported as a format error
		}
		if r := Contrast(fg, bg); r < min {
			errs = append(errs, fmt.Errorf("contrast %s is %.2f:1, below the %.1f:1 minimum (fg %s on bg %s)", label, r, min, fg, bg))
		}
	}
	check("text on surface", t.Tokens.Text, t.Tokens.Surface, 4.5)
	check("muted on surface", t.Tokens.Muted, t.Tokens.Surface, 4.5)
	check("accent on surface (links)", t.Tokens.Accent, t.Tokens.Surface, 4.5)
	check("button label on accent", t.OnAccent(), t.Tokens.Accent, 4.5)
	if t.Light != nil {
		check("light: text on surface", t.Light.Text, t.Light.Surface, 4.5)
		check("light: muted on surface", t.Light.Muted, t.Light.Surface, 4.5)
		check("light: accent on surface (links)", t.Light.Accent, t.Light.Surface, 4.5)
		check("light: button label on accent", t.LightOnAccent(), t.Light.Accent, 4.5)
	}
	return errs
}

// OnAccent is the primary button's label colour: the explicit accent_ink if the
// product states one, otherwise white-or-page-ink, whichever reads better.
// Deriving it removes a token a tenant could get wrong — the retired vtranslate
// theme hard-coded `color: #fff` on an accent scoring 3.16:1 against white, so
// its primary button label failed WCAG AA in every colour scheme.
func (t *Theme) OnAccent() string {
	if t.Tokens.AccentInk != "" {
		return t.Tokens.AccentInk
	}
	return pickInk(t.Tokens.Accent, t.Tokens.Bg)
}

func (t *Theme) LightOnAccent() string {
	if t.Light == nil {
		return ""
	}
	if t.Light.AccentInk != "" {
		return t.Light.AccentInk
	}
	return pickInk(t.Light.Accent, t.Light.Bg)
}

func pickInk(accent, ink string) string {
	if Contrast("#ffffff", accent) >= Contrast(ink, accent) {
		return "#ffffff"
	}
	return ink
}

func RGB(hex string) (r, g, b int) {
	v, _ := strconv.ParseUint(strings.TrimPrefix(hex, "#"), 16, 32)
	return int(v>>16) & 0xff, int(v>>8) & 0xff, int(v) & 0xff
}

func lum(hex string) float64 {
	r, g, b := RGB(hex)
	f := func(c int) float64 {
		s := float64(c) / 255.0
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*f(r) + 0.7152*f(g) + 0.0722*f(b)
}

// Contrast is the WCAG 2.x contrast ratio between two hex colours.
func Contrast(a, b string) float64 {
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// RGBA renders a hex colour as an rgba() literal. Used for focus rings, which
// are DERIVED from the accent rather than being tokens.
func RGBA(hex string, alpha float64) string {
	r, g, b := RGB(hex)
	return fmt.Sprintf("rgba(%d, %d, %d, %s)", r, g, b,
		strings.TrimRight(strings.TrimRight(strconv.FormatFloat(alpha, 'f', 2, 64), "0"), "."))
}
