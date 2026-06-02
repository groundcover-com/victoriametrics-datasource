# CLAUDE.md

Guidance for AI agents (Claude Code) working in this repository.

## Task: recreate the per-architecture, unsigned plugin zips

This fork produces **single-architecture, unsigned** zips for a downstream Grafana
Docker image that `COPY`s and unzips them directly into its plugins directory (it does
**not** use `grafana cli plugins install`, and runs the plugin allowlisted/unsigned).

If a user asks to "build/recreate/regenerate the zips" (or names
`victoriametrics-metrics-datasource-linux-amd64.zip` /
`...-linux-arm64.zip`), follow this. The full rationale and verification details live in
[`docs/build-release-zips.md`](docs/build-release-zips.md) — read it if anything below is
ambiguous, and keep the two docs in sync if the procedure changes.

### Invariants (verify, don't assume)

- Top-level zip directory and binary names are derived from `src/plugin.json`:
  - `id` → the single top-level directory (currently `victoriametrics-metrics-datasource`).
  - `executable` → binary base name (currently `victoriametrics_metrics_backend_plugin`),
    so binaries are `<executable>_linux_amd64` / `<executable>_linux_arm64`.
- Both the frontend (`yarn build`, via `.config/webpack/constants.ts` `DIST_DIR`) and the
  backend (`Magefile.go` `OutputBinaryPath`) build into
  `plugins/victoriametrics-metrics-datasource/` — that directory *is* what gets zipped.
- Each zip carries **exactly one** backend binary (its own arch) and **no** `MANIFEST.txt`.
- Re-read `src/plugin.json` first; if `id`/`executable` changed, update every name below.

### Procedure

Run from the repo root.

1. **Toolchain.** Need Go (per `go.mod`) and Node 24 + Yarn classic v1. `.nvmrc` (`14`)
   is stale — use Node 24 (`mise.toml`/`Dockerfile`). If Node is missing but Docker is
   present, build the frontend with `make frontend-build` instead of step 2. If both are
   missing, bootstrap a standalone Node 24 into `~/.cache/` and put it on `PATH` (see the
   appendix in `docs/build-release-zips.md`).

2. **Frontend:** `yarn install && yarn build`
   → outputs to `plugins/victoriametrics-metrics-datasource/`.

3. **Backend (both arches):**
   ```bash
   OUT="plugins/victoriametrics-metrics-datasource"
   for ARCH in amd64 arm64; do
     CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -trimpath \
       -ldflags '-w -s -extldflags "-static"' \
       -o "$OUT/victoriametrics_metrics_backend_plugin_linux_$ARCH" ./pkg
   done
   ```

4. **Assemble + checksum:** for each arch, copy `plugins/<id>` to a temp staging dir,
   `find ... -delete` the *other* arch's `victoriametrics_metrics_backend_plugin_*`
   binary, then `zip -q -r -X` the `<id>` dir from the staging root into
   `dist-zips/victoriametrics-metrics-datasource-linux-$ARCH.zip`. Then
   `shasum -a 256` the zips into `dist-zips/SHA256SUMS.txt` (+ per-file `.sha256`).
   (Exact commands: steps 3–4 of `docs/build-release-zips.md`.)

5. **Verify and report** (do not claim success without this): for each zip confirm
   exactly one top-level dir `== id`, `plugin.json`/`module.js`/`module.js.map` present,
   exactly one backend binary matching the zip's arch, and zero `MANIFEST.txt`. Use the
   `unzip -l` checks in step 5 of the human doc and show the user the result.

### Notes / gotchas

- Output goes to `dist-zips/` (scratch); the upstream release flow uses `dist/`. Both
  `dist/` and `plugins/` are gitignored — build output is never committed.
- Do **not** prune the lazy-loaded webpack `*.js` chunks / `.map` / `.LICENSE.txt` files;
  they are part of the runtime frontend.
- Do **not** sign the zips or add a `MANIFEST.txt` — these run unsigned by design.
- This is intentionally different from `make vm-plugin-release`, which builds one
  multi-arch (optionally signed) zip. Don't "fix" it to match upstream.
