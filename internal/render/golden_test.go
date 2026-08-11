package render

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BuddhiLW/keycloak-custom/internal/tokens"
)

// update rewrites the committed goldens instead of comparing against them.
// A deliberate flag rather than an env var: regenerating a golden is how a real
// regression gets papered over, so it should be a visible act.
//
//	go test ./internal/render -update
var update = flag.Bool("update", false, "rewrite testdata/golden from the current renderer")

// repoRoot is the checkout root. Tests run with the working directory set to
// the package directory, so this is fixed, not searched.
const repoRoot = "../.."

func goldenDirs(t *testing.T) []string {
	t.Helper()
	glob := filepath.Join(repoRoot, "testdata", "golden", "*", "theme.yaml")
	found, err := filepath.Glob(glob)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatalf("no golden themes under %s — the fixtures are the test", glob)
	}
	dirs := make([]string, len(found))
	for i, f := range found {
		dirs[i] = filepath.Dir(f)
	}
	return dirs
}

func load(t *testing.T, dir string) *tokens.Theme {
	t.Helper()
	th, err := tokens.Load(filepath.Join(dir, "theme.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if errs := th.Validate(); len(errs) > 0 {
		t.Fatalf("fixture does not validate: %v", errs)
	}
	return th
}

// TestGolden is the regression gate the tool's own `render -check` promises:
// the committed stylesheet must be exactly what today's renderer produces from
// the committed theme.yaml. A template edit that changes any tenant's output
// fails here rather than in production.
func TestGolden(t *testing.T) {
	for _, dir := range goldenDirs(t) {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			th := load(t, dir)
			got, err := CSS(th, filepath.Join(repoRoot, "template"))
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			p := filepath.Join(dir, th.Tenant+".css")

			if *update {
				if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Logf("wrote %s (%d bytes)", p, len(got))
				return
			}

			want, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("%v (run: go test ./internal/render -update)", err)
			}
			if got != string(want) {
				t.Errorf("%s is stale or hand-edited\n%s\nrun: go test ./internal/render -update",
					p, firstDiff(string(want), got))
			}
		})
	}
}

// firstDiff reports the first differing line, which is far more useful in CI
// output than dumping two 200-line stylesheets.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return fmt.Sprintf("first difference at line %d:\n  committed: %q\n  rendered:  %q", i+1, wl, gl)
		}
	}
	return "files differ only in trailing bytes"
}

// Render must be deterministic or `kctheme render -check` fails on every run
// and CI learns to ignore it.
func TestRenderIsDeterministic(t *testing.T) {
	for _, dir := range goldenDirs(t) {
		th := load(t, dir)
		a, err := CSS(th, filepath.Join(repoRoot, "template"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := CSS(th, filepath.Join(repoRoot, "template"))
		if err != nil {
			t.Fatal(err)
		}
		if a != b {
			t.Fatalf("%s: two renders of one theme differ", dir)
		}
	}
}

// build.json is what the verifier reads to tell "this tenant's CSS is live"
// from "this tenant fell back to stock", which is otherwise silent.
func TestBuildJSON(t *testing.T) {
	th := load(t, goldenDirs(t)[0])
	css, err := CSS(th, filepath.Join(repoRoot, "template"))
	if err != nil {
		t.Fatal(err)
	}

	j, b, err := BuildJSON(th, css, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte(css))); b.CSSSha != want {
		t.Errorf("css_sha256 %q does not match the rendered stylesheet", b.CSSSha)
	}
	if b.Rendered != "" || strings.Contains(j, "rendered_at") {
		t.Error("the zero timestamp must be omitted: a stamped build.json breaks render -check on every run")
	}
	if b.Tenant != th.Tenant || b.Scheme != string(th.Scheme) || b.Template != th.Template {
		t.Errorf("provenance does not describe the theme: %+v", b)
	}

	// A stated timestamp is carried through in UTC.
	stamp := time.Date(2026, 8, 11, 12, 0, 0, 0, time.FixedZone("BRT", -3*3600))
	if _, b, err = BuildJSON(th, css, stamp); err != nil {
		t.Fatal(err)
	}
	if b.Rendered != "2026-08-11T15:00:00Z" {
		t.Errorf("rendered_at %q is not the UTC instant", b.Rendered)
	}
}

// The ConfigMap is the delivery vehicle. Two properties are load-bearing: the
// stylesheet must survive the YAML block scalar intact, and the production and
// staging objects must differ ONLY in name, because both land in one namespace.
func TestConfigMap(t *testing.T) {
	th := load(t, goldenDirs(t)[0])
	css, err := CSS(th, filepath.Join(repoRoot, "template"))
	if err != nil {
		t.Fatal(err)
	}
	buildJSON, _, err := BuildJSON(th, css, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	prod := ConfigMap(th, css, buildJSON, "")
	staging := ConfigMap(th, css, buildJSON, "-staging")

	if !strings.Contains(prod, "name: kc-theme-"+th.Tenant+"\n") {
		t.Error("production ConfigMap is not named for the tenant")
	}
	if !strings.Contains(staging, "name: kc-theme-"+th.Tenant+"-staging\n") {
		t.Error("staging ConfigMap is not suffixed")
	}
	if strings.Replace(staging, th.Tenant+"-staging", th.Tenant, 1) != prod {
		t.Error("staging and production differ by more than the object name")
	}

	// Round-trip the block scalar by hand: every non-blank stylesheet line must
	// appear indented by exactly four spaces under `theme.css: |`.
	body, ok := cutBlock(prod, "  theme.css: |\n", "  build.json: |\n")
	if !ok {
		t.Fatal("no theme.css block in the ConfigMap")
	}
	var rebuilt []string
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if line == "" {
			rebuilt = append(rebuilt, "")
			continue
		}
		if !strings.HasPrefix(line, "    ") {
			t.Fatalf("stylesheet line is not indented into the block: %q", line)
		}
		rebuilt = append(rebuilt, strings.TrimPrefix(line, "    "))
	}
	if strings.Join(rebuilt, "\n") != strings.TrimRight(css, "\n") {
		t.Error("the stylesheet does not survive the ConfigMap block scalar")
	}
}

func cutBlock(s, start, end string) (string, bool) {
	i := strings.Index(s, start)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// Registration is the one irreducible cross-repo step, so its content is what a
// platform operator pastes into the shared Keycloak CR. Two mistakes there are
// silent: the wrong platform key (a light tenant served darkMode=true) and a
// tenant ConfigMap marked mandatory (a missing tenant object blocking boot).
func TestRegistration(t *testing.T) {
	th := load(t, goldenDirs(t)[0])
	hunk := Registration(th, "")

	if !strings.Contains(hunk, "key: "+th.Scheme.PlatformKey()+"\n") {
		t.Errorf("registration does not project the %q platform key", th.Scheme.PlatformKey())
	}
	if !strings.Contains(hunk, "mountPath: /opt/keycloak/themes/"+th.Tenant) {
		t.Error("registration does not mount the tenant's theme directory")
	}
	// The platform source is mandatory (a theme dir with no theme.properties
	// yields HTTP 500) and the tenant source is optional (an absent tenant
	// ConfigMap must degrade to parent styling, not block the pod).
	platform, ok := cutBlock(hunk, "name: kc-theme-properties\n", "name: kc-theme-")
	if !ok {
		t.Fatal("no platform source in the registration hunk")
	}
	if !strings.Contains(platform, "optional: false") {
		t.Error("the platform ConfigMap must be mandatory")
	}
	if !strings.Contains(hunk[strings.Index(hunk, "name: kc-theme-"+th.Tenant):], "optional: true") {
		t.Error("the tenant ConfigMap must be optional")
	}
	// theme.properties is projected only from the platform source; a tenant key
	// reaching that path is the outage this design exists to prevent.
	if strings.Count(hunk, "path: login/theme.properties") != 1 {
		t.Error("login/theme.properties is projected from more than one source")
	}

	if !strings.Contains(Registration(th, "-staging"), "name: kc-theme-"+th.Tenant+"-staging\n") {
		t.Error("staging registration does not name the staging ConfigMap")
	}
}

// MaxCSS fails loudly long before the API server's 1 MiB ConfigMap cap, which
// would otherwise surface as a rejected kubectl apply in a GitOps sync.
func TestCSSBudget(t *testing.T) {
	th := load(t, goldenDirs(t)[0])
	css, err := CSS(th, filepath.Join(repoRoot, "template"))
	if err != nil {
		t.Fatal(err)
	}
	if len(css) > MaxCSS {
		t.Fatalf("golden stylesheet is %d bytes, over the %d budget", len(css), MaxCSS)
	}

	// An oversized template must be refused rather than shipped.
	dir := t.TempDir()
	big := filepath.Join(dir, th.Template)
	if err := os.MkdirAll(big, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(big, "theme.css.tmpl"),
		[]byte(strings.Repeat("/* filler */\n", MaxCSS/13+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CSS(th, dir); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("oversized stylesheet accepted: %v", err)
	}
}

// A template referencing a field that does not exist must fail the build rather
// than emit "<no value>" into a stylesheet.
func TestMissingTemplateFieldFails(t *testing.T) {
	th := load(t, goldenDirs(t)[0])
	dir := t.TempDir()
	v := filepath.Join(dir, th.Template)
	if err := os.MkdirAll(v, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v, "theme.css.tmpl"),
		[]byte("a { color: {{.Tokens.NoSuchField}}; }"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CSS(th, dir); err == nil {
		t.Fatal("a template naming an unknown field rendered anyway")
	}

	// A missing template version is an error, not an empty stylesheet.
	if _, err := CSS(th, t.TempDir()); err == nil {
		t.Fatal("a missing template rendered anyway")
	}
}

// The generated lockup is embedded in the stylesheet, so brand text must reach
// it escaped — the value layer already rejects markup, and this is the second
// line of that defence.
func TestLogoLockup(t *testing.T) {
	th := load(t, goldenDirs(t)[0])
	th.Brand = tokens.Brand{Mark: "A", Wordmark: "Acme"}
	v, err := newView(th)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v.LogoDataURI, "data:image/svg+xml;base64,") {
		t.Fatalf("lockup is not an embedded SVG: %.40q", v.LogoDataURI)
	}
	if v.LogoCSSValue != fmt.Sprintf("url(%q)", v.LogoDataURI) {
		t.Error("the generated lockup must be a single background layer")
	}
	if v.LogoWidth <= 0 || v.LogoHeight <= 0 {
		t.Error("the lockup has no painted box")
	}

	// No wordmark leaves keycloak.v2's stock logo in place.
	th.Brand = tokens.Brand{}
	if v, err = newView(th); err != nil || v.LogoDataURI != "" || v.LogoCSSValue != "" {
		t.Errorf("an unbranded theme emitted a logo: %q / %v", v.LogoCSSValue, err)
	}
}

// A raster logo is two layers and the first-listed one paints on top, so the
// order IS the mechanism: reversed, the page never shows the real artwork.
// A vector is one layer, because a second sharpens nothing and costs the login
// page a third origin.
func TestRasterLogoLayerOrder(t *testing.T) {
	th := load(t, goldenDirs(t)[0])
	dir := t.TempDir()
	png := append([]byte("\x89PNG\r\n\x1a\n"), 0, 1, 2, 3)
	if err := os.WriteFile(filepath.Join(dir, "p.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	th.BaseDir = dir
	th.Brand = tokens.Brand{Mark: "A", Wordmark: "Acme", Logo: &tokens.Logo{
		Src: "https://cdn.example/logo.webp", Placeholder: "p.png", Width: 120, Height: 40,
	}}

	v, err := newView(th)
	if err != nil {
		t.Fatal(err)
	}
	src := strings.Index(v.LogoCSSValue, "https://cdn.example/logo.webp")
	ph := strings.Index(v.LogoCSSValue, "data:image/png;base64,")
	if src < 0 || ph < 0 {
		t.Fatalf("raster logo is missing a layer: %.80q", v.LogoCSSValue)
	}
	if src > ph {
		t.Error("the full-resolution file must be listed first, or it never covers the placeholder")
	}
	if v.LogoWidth != 120 || v.LogoHeight != 40 {
		t.Errorf("painted box is %dx%d, want the stated 120x40", v.LogoWidth, v.LogoHeight)
	}
	// Real artwork carries its own colours, so there is nothing to repaint for
	// the light scheme.
	if v.LightLogoDataURI != "" {
		t.Error("a raster logo must not be repainted for the light scheme")
	}

	// Vector: the placeholder IS the artwork, and the page fetches no logo.
	th.Brand.Logo.Src = ""
	if v, err = newView(th); err != nil {
		t.Fatal(err)
	}
	if strings.Count(v.LogoCSSValue, "url(") != 1 {
		t.Errorf("a logo with no src must be a single layer: %.80q", v.LogoCSSValue)
	}
}

// A dark-first theme repaints the lockup for the light scheme: the logo is a
// background image, so it cannot inherit currentColor, and without a second URI
// the dark wordmark lands on a near-white card.
func TestLightLockupIsRepainted(t *testing.T) {
	th, err := tokens.Load(filepath.Join(repoRoot, "testdata", "golden", "acme", "theme.yaml"))
	if err != nil {
		t.Skipf("no acme fixture: %v", err)
	}
	th.Scheme = tokens.DarkFirst
	th.Brand = tokens.Brand{Mark: "A", Wordmark: "Acme"}
	th.Light = &tokens.Light{
		Bg: "#ffffff", Surface: "#ffffff", SurfaceAlt: "#ffffff", Border: "#dddddd",
		Text: "#111111", Muted: "#555555", Dim: "#555555", InputBg: "#ffffff",
		Accent: "#1a4fd0", AccentHover: "#143da4", GradientTint: "#eeeeee",
	}
	v, err := newView(th)
	if err != nil {
		t.Fatal(err)
	}
	if v.LightLogoDataURI == "" {
		t.Fatal("dark-first theme emitted no light lockup")
	}
	if v.LightLogoDataURI == v.LogoDataURI {
		t.Error("the light lockup is byte-identical to the dark one")
	}
	if v.LightFocusRing == "" || v.LightOnAccent == "" || v.LightGradient == "" {
		t.Error("light derivations are missing")
	}
}

func TestGradient(t *testing.T) {
	for _, c := range []struct{ kind, tint, want string }{
		{"none", "", "var(--kc-brand-bg)"},
		{"", "", "var(--kc-brand-bg)"},
		{"radial", "#1a2340", "radial-gradient"},
		{"linear", "#e7edf8", "linear-gradient"},
	} {
		got := gradient(c.kind, c.tint)
		if !strings.Contains(got, c.want) {
			t.Errorf("gradient(%q): got %q, want it to contain %q", c.kind, got, c.want)
		}
		if c.tint != "" && !strings.Contains(got, c.tint) {
			t.Errorf("gradient(%q) drops the tint: %q", c.kind, got)
		}
	}
}
