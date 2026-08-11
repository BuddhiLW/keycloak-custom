package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The README documents `new` → `validate` → `render` as the quick start, so a
// scaffold its own validator rejects is a broken front door. Every scheme the
// scaffolder offers must survive that first step untouched.
func TestScaffoldValidates(t *testing.T) {
	for _, scheme := range []string{"dark-first", "light-only"} {
		t.Run(scheme, func(t *testing.T) {
			dir := t.TempDir()
			if err := cmdNew([]string{"-tenant", "acme", "-scheme", scheme, "-dir", dir}); err != nil {
				t.Fatalf("new: %v", err)
			}
			th, err := load(filepath.Join(dir, "theme.yaml"))
			if err != nil {
				t.Fatalf("the scaffold does not pass its own validator:\n%v", err)
			}
			if string(th.Scheme) != scheme {
				t.Errorf("scaffolded scheme %q, want %q", th.Scheme, scheme)
			}

			// And it must render, which is the next line the tool prints.
			t.Setenv("KCTHEME_ROOT", repoRoot(t))
			if err := cmdRender([]string{"-f", filepath.Join(dir, "theme.yaml"), "-out", dir}); err != nil {
				t.Fatalf("render: %v", err)
			}
			for _, p := range []string{
				"dist/login/resources/css/theme.css",
				"dist/login/resources/build.json",
				"manifests/base/configmap.yaml",
				"manifests/staging/configmap.yaml",
			} {
				if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
					t.Errorf("render did not write %s: %v", p, err)
				}
			}

			// -check is the CI gate: it must pass on freshly rendered output and
			// fail the moment a generated file is hand-edited.
			if err := cmdRender([]string{"-f", filepath.Join(dir, "theme.yaml"), "-out", dir, "-check"}); err != nil {
				t.Fatalf("-check failed on freshly rendered output: %v", err)
			}
			css := filepath.Join(dir, "dist/login/resources/css/theme.css")
			b, err := os.ReadFile(css)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(css, append(b, []byte("\n/* hand-edited */\n")...), 0o644); err != nil {
				t.Fatal(err)
			}
			err = cmdRender([]string{"-f", filepath.Join(dir, "theme.yaml"), "-out", dir, "-check"})
			if err == nil {
				t.Fatal("-check accepted a hand-edited stylesheet")
			}
			if !strings.Contains(err.Error(), "theme.css") {
				t.Errorf("-check does not name the drifted file: %v", err)
			}
		})
	}
}

func TestScaffoldRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := cmdNew([]string{"-tenant", "acme", "-dir", dir}); err != nil {
		t.Fatal(err)
	}
	if err := cmdNew([]string{"-tenant", "other", "-dir", dir}); err == nil {
		t.Fatal("a second scaffold overwrote an existing theme.yaml")
	}
	if err := cmdNew([]string{"-dir", dir}); err == nil {
		t.Fatal("-tenant is not required")
	}
}

// root() reports where the checkout is rather than guessing, because a wrong
// guess renders against the wrong template silently.
func TestRootResolution(t *testing.T) {
	t.Run("KCTHEME_ROOT wins", func(t *testing.T) {
		t.Setenv("KCTHEME_ROOT", repoRoot(t))
		got, err := root()
		if err != nil {
			t.Fatal(err)
		}
		if got != repoRoot(t) {
			t.Errorf("got %q, want %q", got, repoRoot(t))
		}
	})

	t.Run("a KCTHEME_ROOT with no template is an error", func(t *testing.T) {
		t.Setenv("KCTHEME_ROOT", t.TempDir())
		if _, err := root(); err == nil || !strings.Contains(err.Error(), "template") {
			t.Fatalf("got %v, want an error naming template/", err)
		}
	})

	t.Run("the working directory is a fallback", func(t *testing.T) {
		t.Setenv("KCTHEME_ROOT", "")
		t.Chdir(repoRoot(t))
		got, err := root()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(got, "template")); err != nil {
			t.Errorf("resolved %q, which has no template/", got)
		}
	})
}

// repoRoot is the checkout root as an absolute path. Tests run with the working
// directory set to the package directory, so this is fixed, not searched.
func repoRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
