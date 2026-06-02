# Building per-architecture plugin zips (forked, unsigned)

This document describes how to build the VictoriaMetrics metrics datasource plugin
from source and package it into **per-architecture zip artifacts** that a downstream
Grafana Docker image can `COPY` and `unzip` directly into its plugins directory.

It is the procedure used to produce:

- `victoriametrics-metrics-datasource-linux-amd64.zip`
- `victoriametrics-metrics-datasource-linux-arm64.zip`

## Why this exists (context)

The upstream release flow (`make vm-plugin-release`, `.github/workflows/release.yaml`)
produces a **single multi-architecture** zip that bundles every platform's backend
binary, and it optionally GPG/Grafana-signs the result.

Our downstream consumer is different:

- It does **not** use `grafana cli plugins install`. It `COPY`s a zip into the image
  and unzips it straight into the Grafana plugins directory, so
  `unzip -d <plugins_dir> <zip>` must create `<plugins_dir>/victoriametrics-metrics-datasource/...`.
- It runs the plugin **unsigned** (allowlisted at runtime via
  `GF_PLUGINS_ALLOW_LOADING_UNSIGNED_PLUGINS`), so **no `MANIFEST.txt` / signature is
  required**.
- Each image is single-architecture, so each zip must carry **only one** backend
  binary (the amd64 zip → only the linux/amd64 binary; arm64 zip → only linux/arm64).

This mirrors how the downstream image already consumes the
`grafana-clickhouse-datasource` plugin.

## Target layout (per zip)

```
victoriametrics-metrics-datasource/            <- exactly one top-level dir, == plugin.json "id"
victoriametrics-metrics-datasource/plugin.json
victoriametrics-metrics-datasource/module.js
victoriametrics-metrics-datasource/module.js.map
victoriametrics-metrics-datasource/<chunk>.js  <- lazy-loaded webpack chunks (+ .map / .LICENSE.txt)
victoriametrics-metrics-datasource/victoriametrics_metrics_backend_plugin_linux_amd64   <- ONLY this arch
victoriametrics-metrics-datasource/img/...
victoriametrics-metrics-datasource/README.md
victoriametrics-metrics-datasource/CHANGELOG.md
victoriametrics-metrics-datasource/LICENSE
```

(VictoriaMetrics ships no `dashboards/` directory, so none is included.)

## Why the build lands in `plugins/<id>/`

Two repo settings make both halves of the build land in the same directory, which is
exactly the directory we zip:

- **Frontend** — `.config/webpack/constants.ts` sets
  `DIST_DIR = 'plugins/victoriametrics-metrics-datasource'`, so `yarn build` writes the
  dist output there (not the scaffold default `dist/`).
- **Backend** — `Magefile.go` overrides
  `cfg.OutputBinaryPath = "plugins/victoriametrics-metrics-datasource"`, and the Grafana
  plugin SDK names each binary `<executable>_<os>_<arch>` using the `executable` field
  from `src/plugin.json`.

The two authoritative names come from `src/plugin.json`:

- `id` = `victoriametrics-metrics-datasource` → the top-level zip directory name.
- `executable` = `victoriametrics_metrics_backend_plugin` → the binary base name, so the
  files are `victoriametrics_metrics_backend_plugin_linux_amd64` / `_linux_arm64`.

If those fields ever change, the directory and binary names below must change with them.

## Prerequisites

- **Go** matching `go.mod` (built with go ≥ 1.25; cross-compiles to linux/amd64 and
  linux/arm64 with `CGO_ENABLED=0`, no cross toolchain needed).
- **Node 24** and **Yarn classic (v1)**. Note: `.nvmrc` says `14` but it is stale — the
  real targets are `mise.toml` (`node = "24"`) and `Dockerfile` (`node:24`). Use Node 24.

If you have Docker but not Node, you can instead build the frontend with
`make frontend-build` (containerized) and skip the manual frontend step. If you have
neither Node nor Docker, see the appendix.

## Steps

Run from the repository root.

### 1. Build the frontend

```bash
yarn install        # also runs the bundled lezer-metricsql preinstall/postinstall
yarn build          # webpack production build -> plugins/victoriametrics-metrics-datasource/
```

### 2. Build the backend for both Linux architectures

We build directly with `go` so each binary is arch-targeted and statically linked. Both
land next to the frontend in `plugins/victoriametrics-metrics-datasource/`.

```bash
OUT="plugins/victoriametrics-metrics-datasource"
LDFLAGS='-w -s -extldflags "-static"'
for ARCH in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath -ldflags "$LDFLAGS" \
    -o "$OUT/victoriametrics_metrics_backend_plugin_linux_$ARCH" ./pkg
done
```

> Alternative: `make vm-backend-plugin-build` (mage) builds **all** platforms with the
> upstream version/build-info ldflags. It works too, but then you must delete the
> non-target binaries from each zip (the loop in step 3 already does this regardless of
> how the binaries were produced).

### 3. Assemble one single-arch zip per architecture

For each arch, stage a copy of the build dir, delete the *other* arch's backend binary,
and zip from the staging root so the only top-level entry is the plugin id directory.

```bash
PLUGIN_ID="victoriametrics-metrics-datasource"
SRC="plugins/$PLUGIN_ID"
OUTDIR="dist-zips"
rm -rf "$OUTDIR" && mkdir -p "$OUTDIR"

for ARCH in amd64 arm64; do
  STAGE="$(mktemp -d)"
  cp -R "$SRC" "$STAGE/$PLUGIN_ID"
  # keep only this arch's backend binary
  find "$STAGE/$PLUGIN_ID" -maxdepth 1 -name 'victoriametrics_metrics_backend_plugin_*' \
    ! -name "victoriametrics_metrics_backend_plugin_linux_$ARCH" -delete
  ZIP="$(pwd)/$OUTDIR/victoriametrics-metrics-datasource-linux-$ARCH.zip"
  ( cd "$STAGE" && zip -q -r -X "$ZIP" "$PLUGIN_ID" )
  rm -rf "$STAGE"
done
```

### 4. Generate checksums

```bash
cd dist-zips
shasum -a 256 victoriametrics-metrics-datasource-linux-*.zip | tee SHA256SUMS.txt
for ARCH in amd64 arm64; do
  Z="victoriametrics-metrics-datasource-linux-$ARCH.zip"
  shasum -a 256 "$Z" > "$Z.sha256"
done
shasum -a 256 -c SHA256SUMS.txt    # should print "OK" for both
cd -
```

> We use **sha256**. The upstream release flow emits **sha1** (`sha1sum`) checksum
> files instead; switch the algorithm if a downstream tool expects sha1.

### 5. Verify

```bash
cd dist-zips
for ARCH in amd64 arm64; do
  Z="victoriametrics-metrics-datasource-linux-$ARCH.zip"
  echo "## $Z"
  # exactly one top-level dir, and it must be the plugin id
  unzip -l "$Z" | awk 'NR>3 && $4!="" {print $4}' | sed -E 's#/.*##' \
    | grep -vE '^(----|[0-9])' | sort -u
  # exactly one backend binary, correct arch
  unzip -l "$Z" | grep -E 'victoriametrics_metrics_backend_plugin'
  # key frontend files present
  unzip -l "$Z" | grep -E '/(plugin\.json|module\.js|module\.js\.map)$'
  # confirm unsigned (no signature manifest)
  echo "MANIFEST.txt count: $(unzip -l "$Z" | grep -c MANIFEST.txt)   # expect 0"
done
cd -
```

Expected for each zip: a single top-level `victoriametrics-metrics-datasource/`,
`plugin.json` + `module.js` + `module.js.map` present, exactly one
`victoriametrics_metrics_backend_plugin_linux_<arch>` matching the zip's arch, and zero
`MANIFEST.txt`.

## Notes

- `dist-zips/` is a scratch output directory chosen to avoid colliding with the upstream
  release flow, which uses `dist/`. Both `dist/` and `plugins/` are gitignored, so none
  of the build output is committed.
- Each zip contains the full webpack output (the lazy-loaded `*.js` chunks and their
  `.map` / `.LICENSE.txt` files are part of the runtime frontend — do not prune them).

## Appendix: no Node and no Docker on the build host

The frontend build needs Node 24 + Yarn classic. If neither Node nor Docker is
available, fetch a standalone Node 24 build into a scratch dir (no system install) and
add it to `PATH` for the build session. Example for macOS arm64:

```bash
BD="$HOME/.cache/cc-node24"; mkdir -p "$BD"; cd "$BD"
VER=$(curl -fsSL https://nodejs.org/dist/index.json \
  | grep -o '"version":"v24[0-9.]*"' | head -1 | sed 's/.*"v/v/;s/"//')
TAR="node-${VER}-darwin-arm64.tar.gz"
curl -fsSLO "https://nodejs.org/dist/${VER}/${TAR}" && tar -xzf "$TAR"
export PATH="$BD/node-${VER}-darwin-arm64/bin:$PATH"
npm install -g yarn@1.22.22     # installs into the standalone Node prefix only
cd -
```

Pick the matching `node-v24.*-<os>-<arch>.tar.{gz,xz}` for other platforms. After this,
`node`, `npm`, and `yarn` are on `PATH` for the current shell and the steps above work
unchanged.
