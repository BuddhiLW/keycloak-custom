// Package verify boots a real Keycloak and asserts the theme resolves. It
// exists because every cheaper check passes on a theme that silently falls back
// to stock: Keycloak logs the miss at ERROR and then serves a perfectly good
// unbranded page.
package verify

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const Image = "quay.io/keycloak/keycloak:26.6.1"

type Result struct {
	Steps []string
	Err   error
}

func (r *Result) step(f string, a ...any) { r.Steps = append(r.Steps, fmt.Sprintf(f, a...)) }

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// Local boots stock Keycloak with the platform theme.properties and this
// tenant's rendered dist/, then asserts BOTH stylesheets are served: the
// parent's css/styles.css and the brand css/theme.css. Asserting only the brand
// sheet is what let the retired themes ship with `styles=` silently dropping
// the parent's stylesheet.
func Local(tenant, distDir, platformProps, scheme string, keep bool) *Result {
	r := &Result{}
	cname := "kctheme-verify-" + tenant

	_, _ = run("docker", "rm", "-f", cname)

	tmp, err := os.MkdirTemp("", "kctheme-")
	if err != nil {
		r.Err = err
		return r
	}
	if !keep {
		defer os.RemoveAll(tmp)
	}

	// Build the exact directory the pod will see: theme.properties from the
	// platform ConfigMap source, everything else from the tenant's dist/.
	themeDir := filepath.Join(tmp, tenant)
	if err := os.MkdirAll(filepath.Join(themeDir, "login", "resources", "css"), 0o755); err != nil {
		r.Err = err
		return r
	}
	props, err := extractKey(platformProps, scheme)
	if err != nil {
		r.Err = err
		return r
	}
	if err := os.WriteFile(filepath.Join(themeDir, "login", "theme.properties"), []byte(props), 0o644); err != nil {
		r.Err = err
		return r
	}
	if out, err := run("cp", "-r", distDir+"/login/.", filepath.Join(themeDir, "login")+"/"); err != nil {
		r.Err = fmt.Errorf("copy dist: %v: %s", err, out)
		return r
	}
	// os.MkdirTemp is 0700. The container runs as uid 1000 in its own user
	// namespace, so without this the whole tree is unreadable and Keycloak
	// silently serves built-in themes — it logs "Failed to find LOGIN theme"
	// at ERROR and then returns a perfectly good unbranded page.
	if err := chmodTree(tmp); err != nil {
		r.Err = err
		return r
	}
	r.step("built theme dir %s (theme.properties from platform key %q)", themeDir, scheme)

	out, err := run("docker", "run", "-d", "--name", cname,
		"-e", "KC_BOOTSTRAP_ADMIN_USERNAME=admin",
		"-e", "KC_BOOTSTRAP_ADMIN_PASSWORD=admin",
		"-p", "0:8080",
		"-v", tmp+":/opt/keycloak/themes:ro",
		"-m", "2g", "--cpus", "1",
		Image, "start-dev")
	if err != nil {
		r.Err = fmt.Errorf("docker run: %v: %s", err, out)
		return r
	}
	if !keep {
		defer run("docker", "rm", "-f", cname)
	}

	port, err := hostPort(cname)
	if err != nil {
		r.Err = err
		return r
	}
	base := "http://127.0.0.1:" + port
	r.step("container %s up on %s", cname, base)

	// Generous: `start-dev` re-runs Quarkus augmentation on every boot, which on
	// a single pinned CPU regularly exceeds two minutes. A tight budget here
	// produces a flaky "verification failed" that teaches people to ignore it.
	if err := waitReady(base, 300*time.Second); err != nil {
		logs, _ := run("docker", "logs", "--tail", "40", cname)
		r.Err = fmt.Errorf("%w\n--- logs ---\n%s", err, logs)
		return r
	}
	r.step("server ready")

	kcadm := func(args ...string) (string, error) {
		full := append([]string{"exec", cname, "/opt/keycloak/bin/kcadm.sh"}, args...)
		return run("docker", full...)
	}
	if out, err := kcadm("config", "credentials", "--server", "http://localhost:8080",
		"--realm", "master", "--user", "admin", "--password", "admin"); err != nil {
		r.Err = fmt.Errorf("kcadm login: %v: %s", err, out)
		return r
	}
	if out, err := kcadm("update", "realms/master", "-s", "loginTheme="+tenant); err != nil {
		r.Err = fmt.Errorf("set loginTheme=%s: %v: %s", tenant, err, out)
		return r
	}
	r.step("master realm loginTheme=%s", tenant)

	page, err := loginPage(base)
	if err != nil {
		r.Err = err
		return r
	}
	if strings.Contains(page, "we are sorry") || strings.Contains(page, "Internal Server Error") {
		r.Err = fmt.Errorf("login page rendered an error page; theme.properties is probably malformed")
		return r
	}

	// Both sheets, in order. The parent sheet proves `styles=` did not clobber
	// keycloak.v2; the brand sheet proves this tenant's CSS is actually live.
	hrefs := cssHrefs(page)
	var parent, brand string
	for _, h := range hrefs {
		if strings.Contains(h, "/css/styles.css") {
			parent = h
		}
		if strings.Contains(h, "/css/theme.css") {
			brand = h
		}
	}
	if parent == "" {
		r.Err = fmt.Errorf("parent stylesheet css/styles.css is NOT linked — `styles=` in theme.properties overrode instead of appending; found: %v", hrefs)
		return r
	}
	if brand == "" {
		r.Err = fmt.Errorf("brand stylesheet css/theme.css is NOT linked; found: %v", hrefs)
		return r
	}
	for label, href := range map[string]string{"parent css/styles.css": parent, "brand css/theme.css": brand} {
		code, n, err := fetch(base + href)
		if err != nil {
			r.Err = err
			return r
		}
		if code != 200 {
			r.Err = fmt.Errorf("%s returned HTTP %d (%s)", label, code, href)
			return r
		}
		r.step("%s -> 200, %d bytes (%s)", label, n, href)
	}

	// A theme that never resolved logs this and then serves a fine-looking page.
	logs, _ := run("docker", "logs", cname)
	if strings.Contains(logs, "Failed to find LOGIN theme") {
		r.Err = fmt.Errorf("server logged \"Failed to find LOGIN theme\" — the theme did not resolve")
		return r
	}
	r.step("no theme-resolution errors in server log")
	return r
}

// chmodTree makes the rendered theme world-readable and its directories
// world-traversable, matching the 0444 defaultMode the shared Keycloak CR uses
// for the real projected volume.
func chmodTree(root string) error {
	return filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if fi.IsDir() {
			mode = 0o755
		}
		return os.Chmod(p, mode)
	})
}

func extractKey(platformProps, key string) (string, error) {
	b, err := os.ReadFile(platformProps)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == key+": |" {
			var out []string
			for _, n := range lines[i+1:] {
				if strings.HasPrefix(n, "    ") {
					out = append(out, strings.TrimPrefix(n, "    "))
					continue
				}
				if strings.TrimSpace(n) == "" {
					continue
				}
				break
			}
			return strings.Join(out, "\n") + "\n", nil
		}
	}
	return "", fmt.Errorf("key %q not found in %s", key, platformProps)
}

func hostPort(cname string) (string, error) {
	out, err := run("docker", "port", cname, "8080/tcp")
	if err != nil {
		return "", fmt.Errorf("docker port: %v: %s", err, out)
	}
	f := strings.Fields(strings.TrimSpace(strings.Split(out, "\n")[0]))
	last := f[len(f)-1]
	i := strings.LastIndex(last, ":")
	return last[i+1:], nil
}

func waitReady(base string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if code, _, err := fetch(base + "/realms/master/.well-known/openid-configuration"); err == nil && code == 200 {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("server did not become ready within %s", d)
}

func fetch(url string) (int, int, error) {
	out, err := run("curl", "-sS", "-m", "20", "-o", "/tmp/kctheme-body", "-w", "%{http_code} %{size_download}", url)
	if err != nil {
		return 0, 0, fmt.Errorf("curl %s: %v: %s", url, err, out)
	}
	var code, n int
	fmt.Sscanf(strings.TrimSpace(out), "%d %d", &code, &n)
	return code, n, nil
}

func loginPage(base string) (string, error) {
	u := base + "/realms/master/protocol/openid-connect/auth" +
		"?client_id=security-admin-console&response_type=code&scope=openid" +
		"&redirect_uri=" + base + "/admin/master/console/" +
		"&code_challenge_method=S256&code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	out, err := run("curl", "-sSL", "-m", "20", u)
	if err != nil {
		return "", fmt.Errorf("fetch login page: %v: %s", err, out)
	}
	return out, nil
}

func cssHrefs(page string) []string {
	var out []string
	for _, part := range strings.Split(page, `<link`)[1:] {
		end := strings.Index(part, ">")
		if end < 0 {
			continue
		}
		tag := part[:end]
		if !strings.Contains(tag, "stylesheet") && !strings.Contains(tag, ".css") {
			continue
		}
		i := strings.Index(tag, `href="`)
		if i < 0 {
			continue
		}
		rest := tag[i+6:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			continue
		}
		out = append(out, rest[:j])
	}
	return out
}
