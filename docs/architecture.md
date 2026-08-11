# Architecture

## The problem

Two products need different login pages. Both authenticate against the same Keycloak.

The conventional answer is a themed image per product — `FROM quay.io/keycloak/keycloak`,
`COPY theme/`, build, push, and now every brand tweak is an image build, a registry push,
a CR bump and a rolling restart of the shared instance. Worse, the instance is shared, so
one tenant's colour change restarts every tenant's authentication.

`kctheme` keeps the stock image and moves the theme into ConfigMaps.

## Delivery path

```
theme.yaml            (tenant repo, hand-edited)
   │  kctheme render
   ▼
dist/                 theme.css + build.json
manifests/            ConfigMap kc-theme-<tenant>
   │  git push → GitOps
   ▼
ConfigMap in the keycloak namespace
   │  projected volume, declared once in the shared Keycloak CR
   ▼
/opt/keycloak/themes/<tenant>/login/
   ├── theme.properties          ← from kc-theme-properties  (PLATFORM)
   └── resources/
       ├── css/theme.css         ← from kc-theme-<tenant>    (TENANT)
       └── build.json            ← from kc-theme-<tenant>    (TENANT)
```

One projected volume, two ConfigMap sources, different owners. That split is the whole
design.

The instance runs `startOptimized: false` so themes are read from disk at request time;
no image rebuild is involved at any point.

## Ownership

| repo | owns | may change without touching the others |
|---|---|---|
| this one | the generator, the stylesheet template, `kc-theme-properties` | template versions |
| tenant theme repo | `theme.yaml`, brand assets | **colours, logo, typography** |
| platform repo (owns the Keycloak CR) | the projected volume + mount per tenant | adding/removing a tenant |

A **styling** change touches exactly one repo. Only **onboarding a tenant** or **changing
`scheme` across the dark/light boundary** requires a platform PR, because both change
which keys are projected.

This tool owns themes and nothing else. Realms and clients are a separate concern with a
separate tool; neither depends on the other, and no product name appears in this module's
import path.

## Theme resolution

Keycloak picks a login theme from two places, narrower wins:

1. `RealmRepresentation.loginTheme` — per realm
2. `ClientRepresentation.attributes["login_theme"]` — per client

Per-client is the finer grain and is what lets one realm front several products.

## Threat model

The shared instance is the asset. A tenant repo is the untrusted input — it is edited by
whoever owns that product's brand, not by whoever operates Keycloak.

### The attack

`login/theme.properties` carries `parent=`, `styles=`, `import=` and `meta=`. Java's
`Properties` format is **last-wins**. A tenant who can write a second `parent=` line
builds an unbounded parent chain:

```
100% CPU → OutOfMemoryError → JVM Exit=3 → kubelet restart
→ poison still mounted → traffic-driven crashloop
```

Not one tenant's login page — *every* tenant's authentication, until someone identifies
the mounted ConfigMap and removes it by hand.

Two independent adversarial reviews arrived at this same outage, and both produced attack
files that passed every substring-based admission rule that could be written against
them. Validation loses here: the input is a Java properties file with last-wins
semantics, and the check has to be exhaustive while the attack only has to be novel.

### The mitigation

Do not validate the path — remove it.

`theme.properties` is **not tenant data**. The shared Keycloak CR projects it from the
platform-owned `kc-theme-properties` ConfigMap, whose `items[]` are fixed:

```yaml
sources:
  - configMap:
      name: kc-theme-properties       # PLATFORM — not writable by a tenant
      optional: false
      items: [{key: dark, path: login/theme.properties}]
  - configMap:
      name: kc-theme-<tenant>         # TENANT
      optional: true
      items:
        - {key: theme.css,  path: login/resources/css/theme.css}
        - {key: build.json, path: login/resources/build.json}
```

Two facts make the attack unreachable rather than merely blocked:

1. **ConfigMap keys cannot contain `/`.** A tenant cannot name a key `login/theme.properties`.
2. **kubelet writes only the keys named in `items[]`.** Extra keys in a tenant ConfigMap
   are not projected at all.

So no expression available to a tenant reaches `theme.properties`. `theme.yaml` has no
field for `parent=` because there is nowhere for such a field to go.

`optional: false` on the platform source is deliberate: if that ConfigMap is missing the
pod must fail to start, rather than start with a tenant directory that has no
`theme.properties` at all.

### Residual surface

A tenant still supplies CSS, so the value-layer regexes in `internal/tokens` matter: they
exist to stop a token closing its declaration and opening a new one. `card.shadow`
excludes `;{}:@\` and `/*`; `brand.logo.src` excludes quotes, parens, backslash,
semicolon and whitespace, because it lands inside `url("…")`; `font.import_url` is
restricted to `fonts.googleapis.com` over https. These are the same class of escape as
the `parent=` attack, one layer down — but the blast radius is one tenant's login page,
not the JVM.

## Why `parent=keycloak.v2`

Not `base`. The commonly repeated reason — that `parent=base` returns 500 — is false;
measured on 26.6.1 it renders a degraded 4942-byte page. The real reason is that the
generated brand CSS targets PatternFly v5 selectors that the `base` theme never emits.

## Cache epoch

Theme resources are served `Cache-Control: max-age=2592000` with no `ETag` and no
`Last-Modified`, and `/resources/<hash>/` is per-installation and survives restarts. The
`?v=N` suffix on `styles=` in `kc-theme-properties` is therefore the **only** thing that
busts a returning browser's cache. Bumping it is a platform action affecting all tenants
and needs a rolling restart, since `theme.properties` is JVM-cached.

Note also that `styles=` is a property **override**, not an append: listing only the
brand sheet silently drops `keycloak.v2`'s `css/styles.css`. Both keys list both sheets.

## Verification

`kctheme verify -local` boots the stock Keycloak image in docker with the rendered
`dist/` and the platform properties file mounted the way the CR mounts them, then asserts
the theme resolves and both stylesheets are served — catching a fallback to the default
theme, which otherwise looks like "the CSS didn't apply".

`kctheme render -check` re-renders and fails if committed output differs from
`theme.yaml`, so generated files cannot be hand-edited without CI noticing.
