# kctheme

Per-tenant Keycloak login themes on a **shared** Keycloak instance — without building a
custom Keycloak image.

The usual answer to "our two products need different login pages" is to bake a themed
image per product and run a Keycloak per product. `kctheme` takes the other path: one
stock `quay.io/keycloak/keycloak` pod, one `/opt/keycloak/themes` directory, and each
tenant's theme projected in from ConfigMaps. A tenant repo holds brand **tokens**; the
stylesheet, the ConfigMap and the registration hunk are generated.

```
go install github.com/BuddhiLW/keycloak-custom/cmd/kctheme@latest
```

## Quick start

```console
$ kctheme new -tenant acme -scheme light-only
wrote theme.yaml
next: edit the tokens, then `kctheme render` and `kctheme verify -local`

$ $EDITOR theme.yaml          # brand colours, radius, shadow, logo

$ kctheme validate
ok  theme.yaml  tenant=acme scheme=light-only template=v1
    platform theme.properties key: "light"
    light  text/surface 17.26:1  links 4.57:1  button label #ffffff on accent 4.57:1

$ kctheme render
wrote dist/login/resources/css/theme.css (6667 bytes)
wrote dist/login/resources/build.json (153 bytes)
wrote manifests/base/configmap.yaml (8139 bytes)
wrote manifests/staging/configmap.yaml (8147 bytes)

$ kctheme verify -local       # boots stock Keycloak in docker, asserts the theme resolves
PASS — theme resolves, both stylesheets served, no fallback.
```

`kctheme register` then prints the one hunk a platform operator applies to the shared
Keycloak CR. After that, a **styling** change never touches the platform repo again —
edit tokens, `render`, commit, and GitOps carries it.

## Verbs

| verb | what it does |
|---|---|
| `new -tenant X -scheme dark-first\|light-only [-dir .]` | scaffold `theme.yaml` |
| `validate [-f theme.yaml]` | schema + WCAG contrast gate |
| `render [-f theme.yaml] [-out .] [-check]` | generate `dist/` and `manifests/`; `-check` fails on drift |
| `verify -local [-f] [-dist] [-keep]` | boot stock Keycloak in docker and assert resolution |
| `register [-f] [-env production\|staging]` | print the CR patch for the platform repo |

`render -check` is the CI gate: it re-renders and fails if the committed output differs
from `theme.yaml`, so generated files cannot be hand-edited without detection.

## Why a tenant cannot break the shared instance

This is the design's load-bearing claim, so it is worth stating precisely.

A Keycloak theme is configured by `login/theme.properties`. That file carries `parent=`,
`styles=`, `import=` and `meta=`. Java's `Properties` format is **last-wins**, so a
tenant able to write a second `parent=` line can build an unbounded parent chain: 100%
CPU, `OutOfMemoryError`, JVM `Exit=3`, kubelet restart, poison still mounted — a
traffic-driven crashloop taking down *every* tenant's authentication. Two independent
adversarial reviews produced attack files that passed every substring-based admission
rule that could be written against them.

The mitigation is not a better validator. `theme.properties` is **not tenant data**: it
is projected from the platform-owned `kc-theme-properties` ConfigMap
([`platform/`](platform/kc-theme-properties.yaml)) by the shared Keycloak CR. ConfigMap
keys cannot contain `/`, and kubelet writes only the keys named in the CR's `items[]`.
A tenant repo therefore has **no expression that reaches that path** — `parent=`,
`styles=`, `meta=` and `import=` are unreachable, not merely validated.

That is why `theme.yaml` has no field for them, and why `scheme:` selects *which platform
key* is projected rather than emitting properties itself. Changing `scheme` is a
registration change (a PR against the repo owning the CR, plus a rolling restart);
changing colours is not.

## Repository layout

```
cmd/kctheme/      the CLI
internal/tokens/  theme.yaml schema, validation, WCAG contrast
internal/render/  stylesheet, build.json, ConfigMap, CR registration hunk
internal/verify/  docker-based resolution probe against stock Keycloak
template/v1/      the stylesheet template (versioned; v1 is stable)
platform/         the platform-owned ConfigMap — the security boundary
testdata/golden/  rendered reference output
```

## Scope

The generated stylesheet covers design tokens, page chrome, the login card, the primary
button and text inputs. It deliberately does **not** restyle error banners, alerts, or
the OTP/recovery templates: those shift between Keycloak minors, and a theme reaching
into them breaks silently on upgrade — discovered only when someone cannot log in.

Targets Keycloak **26.6.x** with `parent=keycloak.v2` (PatternFly v5).

## Documentation

- [docs/architecture.md](docs/architecture.md) — delivery path, ownership split, threat model
- [docs/theme-yaml.md](docs/theme-yaml.md) — every `theme.yaml` field

## License

Apache-2.0. See [LICENSE](LICENSE).
