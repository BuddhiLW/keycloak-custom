// kctheme scaffolds, validates, renders and verifies per-project Keycloak login
// themes for a SHARED Keycloak instance.
//
// It is deliberately vendor-neutral: no product name appears in this module's
// path. kcctl owns realms and clients; kctheme owns themes. Neither depends on
// the other.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BuddhiLW/keycloak-custom/internal/render"
	"github.com/BuddhiLW/keycloak-custom/internal/tokens"
	"github.com/BuddhiLW/keycloak-custom/internal/verify"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "new":
		err = cmdNew(os.Args[2:])
	case "validate":
		err = cmdValidate(os.Args[2:])
	case "render":
		err = cmdRender(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "register":
		err = cmdRegister(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown verb %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\033[1;31merror:\033[0m %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `kctheme — per-project Keycloak login themes on a shared instance

  kctheme new      -tenant X -scheme dark-first|light-only [-dir .]
  kctheme validate [-f theme.yaml]
  kctheme render   [-f theme.yaml] [-out .] [-check]
  kctheme verify   -local [-f theme.yaml] [-dist dist] [-keep]
  kctheme register [-f theme.yaml] [-env production|staging]

A theme repo holds brand TOKENS only. The stylesheet, the ConfigMap and the
registration hunk are generated. login/theme.properties is NOT tenant data — it
is projected from a platform-owned ConfigMap by the shared Keycloak CR, which is
what makes parent=, styles=, meta= and import= unreachable from a theme repo.
`)
}

// root locates the keycloak-custom checkout so `template/` and `platform/` can
// be found whether kctheme is run from a tenant repo or from source. It searches
// $KCTHEME_ROOT, then up to four parents of the executable, then the working
// directory, and reports an error rather than guessing a path.
func root() (string, error) {
	if v := os.Getenv("KCTHEME_ROOT"); v != "" {
		if _, err := os.Stat(filepath.Join(v, "template")); err != nil {
			return "", fmt.Errorf("KCTHEME_ROOT=%s has no template/ directory", v)
		}
		return v, nil
	}
	var tried []string
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		for i := 0; i < 4; i++ {
			if _, err := os.Stat(filepath.Join(d, "template")); err == nil {
				return d, nil
			}
			tried = append(tried, d)
			d = filepath.Dir(d)
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if _, err := os.Stat(filepath.Join(wd, "template")); err == nil {
			return wd, nil
		}
		tried = append(tried, wd)
	}
	return "", fmt.Errorf("cannot locate the keycloak-custom checkout (no template/ found in %s); set KCTHEME_ROOT",
		strings.Join(tried, ", "))
}

func load(path string) (*tokens.Theme, error) {
	t, err := tokens.Load(path)
	if err != nil {
		return nil, err
	}
	if errs := t.Validate(); len(errs) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "%s is invalid (%d problems):", path, len(errs))
		for _, e := range errs {
			fmt.Fprintf(&sb, "\n  - %v", e)
		}
		return nil, fmt.Errorf("%s", sb.String())
	}
	return t, nil
}

func cmdValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	f := fs.String("f", "theme.yaml", "path to theme.yaml")
	fs.Parse(args)
	t, err := load(*f)
	if err != nil {
		return err
	}
	fmt.Printf("ok  %s  tenant=%s scheme=%s template=%s\n", *f, t.Tenant, t.Scheme, t.Template)
	fmt.Printf("    platform theme.properties key: %q\n", t.Scheme.PlatformKey())
	ink := t.OnAccent()
	// The base tokens ARE the light palette for a light-only tenant; labelling
	// them "dark" reads as a mis-authored theme to whoever runs this.
	base := "dark "
	if t.Scheme == tokens.LightOnly {
		base = "light"
	}
	fmt.Printf("    %s  text/surface %.2f:1  links %.2f:1  button label %s on accent %.2f:1\n",
		base,
		tokens.Contrast(t.Tokens.Text, t.Tokens.Surface),
		tokens.Contrast(t.Tokens.Accent, t.Tokens.Surface),
		ink, tokens.Contrast(ink, t.Tokens.Accent))
	if t.Light != nil {
		link := t.LightOnAccent()
		fmt.Printf("    light  text/surface %.2f:1  links %.2f:1  button label %s on accent %.2f:1\n",
			tokens.Contrast(t.Light.Text, t.Light.Surface),
			tokens.Contrast(t.Light.Accent, t.Light.Surface),
			link, tokens.Contrast(link, t.Light.Accent))
	}
	return nil
}

func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	f := fs.String("f", "theme.yaml", "path to theme.yaml")
	out := fs.String("out", ".", "repo root to write dist/ and manifests/ into")
	check := fs.Bool("check", false, "re-render and fail if the committed output differs")
	fs.Parse(args)

	t, err := load(*f)
	if err != nil {
		return err
	}
	rootDir, err := root()
	if err != nil {
		return err
	}
	css, err := render.CSS(t, filepath.Join(rootDir, "template"))
	if err != nil {
		return err
	}
	// No timestamp: render must be deterministic, or `render --check` would fail
	// on every run and CI would learn to ignore it.
	buildJSON, b, err := render.BuildJSON(t, css, time.Time{})
	if err != nil {
		return err
	}

	files := map[string]string{
		filepath.Join(*out, "dist/login/resources/css/theme.css"): css,
		filepath.Join(*out, "dist/login/resources/build.json"):    buildJSON,
		filepath.Join(*out, "manifests/base/configmap.yaml"):      render.ConfigMap(t, css, buildJSON, ""),
		filepath.Join(*out, "manifests/staging/configmap.yaml"):   render.ConfigMap(t, css, buildJSON, "-staging"),
	}

	if *check {
		var drift []string
		for p, want := range files {
			got, err := os.ReadFile(p)
			if err != nil {
				drift = append(drift, p+" (missing)")
				continue
			}
			if string(got) != want {
				drift = append(drift, p+" (differs)")
			}
		}
		if len(drift) > 0 {
			return fmt.Errorf("generated output is stale or hand-edited:\n  - %s\nrun: kctheme render", strings.Join(drift, "\n  - "))
		}
		fmt.Printf("ok  generated output matches theme.yaml (css sha256 %s)\n", b.CSSSha[:12])
		return nil
	}

	for p, c := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d bytes)\n", p, len(c))
	}
	fmt.Printf("css sha256 %s\n", b.CSSSha)
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	local := fs.Bool("local", false, "boot stock Keycloak in docker and assert the theme resolves")
	f := fs.String("f", "theme.yaml", "path to theme.yaml")
	dist := fs.String("dist", "dist", "rendered dist/ directory")
	keep := fs.Bool("keep", false, "leave the container running for inspection")
	fs.Parse(args)
	if !*local {
		return fmt.Errorf("only -local is implemented; the in-cluster probe runs as the platform CronJob")
	}
	t, err := load(*f)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is required for -local")
	}
	rootDir, err := root()
	if err != nil {
		return err
	}
	props := filepath.Join(rootDir, "platform", "kc-theme-properties.yaml")
	fmt.Printf("booting %s with tenant %q (scheme %s)...\n", verify.Image, t.Tenant, t.Scheme)
	r := verify.Local(t.Tenant, *dist, props, t.Scheme.PlatformKey(), *keep)
	for _, s := range r.Steps {
		fmt.Println("  ✓ " + s)
	}
	if r.Err != nil {
		return r.Err
	}
	fmt.Println("\nPASS — theme resolves, both stylesheets served, no fallback.")
	return nil
}

func cmdRegister(args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	f := fs.String("f", "theme.yaml", "path to theme.yaml")
	env := fs.String("env", "production", "production|staging")
	fs.Parse(args)
	t, err := load(*f)
	if err != nil {
		return err
	}
	suffix := ""
	cr := "Keycloak/keycloak"
	if *env == "staging" {
		suffix, cr = "-staging", "Keycloak/staging-keycloak"
	}
	fmt.Printf("# Add to %s spec.unsupported.podTemplate in the repo that owns the CR.\n", cr)
	fmt.Printf("# This is the one irreducible cross-repo step; a STYLING change needs none of it.\n\n")
	fmt.Print(render.Registration(t, suffix))
	return nil
}

func cmdNew(args []string) error {
	fs := flag.NewFlagSet("new", flag.ExitOnError)
	tenant := fs.String("tenant", "", "tenant/theme name (lowercase, becomes a directory and ConfigMap suffix)")
	scheme := fs.String("scheme", "dark-first", "dark-first|light-only")
	dir := fs.String("dir", ".", "directory to scaffold into")
	fs.Parse(args)
	if *tenant == "" {
		return fmt.Errorf("-tenant is required")
	}
	light := ""
	if *scheme == "dark-first" {
		light = `
# Only for scheme: dark-first — the prefers-color-scheme: light override.
#
# accent and accent_hover are repeated rather than inherited: one accent cannot
# serve both schemes accessibly. The dark accent above scores 5.39:1 on the dark
# surface and 3.16:1 on white, so a light mode that inherited it would ship a
# link colour below WCAG AA.
light:
  bg: "#f4f6fb"
  surface: "#ffffff"
  border: "#dbe1ee"
  text: "#111629"
  muted: "#5b6580"
  input_bg: "#fbfcff"
  accent: "#2a5bd7"
  accent_hover: "#1f47ad"
  gradient_tint: "#e8edfb"
`
	}
	y := fmt.Sprintf(`# THE ONLY HAND-EDITED FILE IN THIS REPO.
# Everything under dist/ and manifests/ is generated by "kctheme render".
#
# You cannot set parent=, styles=, meta= or import= here, and that is deliberate:
# login/theme.properties is projected from a platform-owned ConfigMap by the
# shared Keycloak CR, so a theme repo has no expression that reaches it.

tenant: %s

# dark-first | light-only. Selects WHICH platform theme.properties key is
# projected, so changing it is a registration change (a PR to the repo owning
# the Keycloak CR, plus a rolling restart), not a content change.
scheme: %s

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

page:
  gradient:
    kind: radial     # radial | linear
    tint: "#1a2340"

card:
  radius: "14px"
  shadow: "0 20px 60px rgba(0, 0, 0, 0.45)"
%s`, *tenant, *scheme, light)

	p := filepath.Join(*dir, "theme.yaml")
	if _, err := os.Stat(p); err == nil {
		return fmt.Errorf("%s already exists", p)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(y), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\nnext: edit the tokens, then `kctheme render` and `kctheme verify -local`\n", p)
	return nil
}
