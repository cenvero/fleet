# Vendored files

The web UI's Terminal view draws with [xterm.js](https://github.com/xtermjs/xterm.js)
(MIT). The files here are copied from the published npm packages so the page loads
nothing from outside (`script-src 'self'`). Only the four files listed below are
embedded in the binary (`internal/webui/assets.go`); this note and the licenses are not.

| File | Package | Path in package |
|---|---|---|
| `xterm.js` | `@xterm/xterm@5.5.0` | `lib/xterm.js` |
| `xterm.css` | `@xterm/xterm@5.5.0` | `css/xterm.css` |
| `xterm-addon-fit.js` | `@xterm/addon-fit@0.10.0` | `lib/addon-fit.js` |
| `xterm-addon-webgl.js` | `@xterm/addon-webgl@0.18.0` | `lib/addon-webgl.js` |

## One modification

The three `.js` files are byte-identical to the published ones except that the final
`//# sourceMappingURL=…` comment is removed (the `.map` files are not shipped).
`xterm.css` is unchanged.

## Integrity

npm `dist.integrity` of each package tarball, as published on registry.npmjs.org:

| Package | Integrity |
|---|---|
| `@xterm/xterm@5.5.0` | `sha512-hqJHYaQb5OptNunnyAnkHyM8aCjZ1MEIDTQu1iIbbTD/xops91NB5yq1ZK/dC2JDbVWtF23zUtl9JE2NqwT87A==` |
| `@xterm/addon-fit@0.10.0` | `sha512-UFYkDm4HUahf2lnEyHvio51TNGiLK66mqP2JoATy7hRZeXaGMRDr00JiSF7m63vR5WKATF605yEggJKsw0JpMQ==` |
| `@xterm/addon-webgl@0.18.0` | `sha512-xCnfMBTI+/HKPdRnSOHaJDRqEpq2Ugy8LEj9GiY4J3zJObo3joylIFaMvzBwbYRg8zLtkO0KQaStCeSfoaI2/w==` |

SHA-256 of each file as published, and as vendored here:

| File | Published | Vendored |
|---|---|---|
| `xterm.js` | `1f991ac3b4b283ebf96e60ae23a00a52765dd3a2e46fa6fdda9f1aab032f7495` | `7a88e46a58847d3f17109e3d48cc8aa056a01997d00ceb76035f63370712d858` |
| `xterm-addon-fit.js` | `bdaefa370b1bfc42ee88d46fe6072400902a4d4b2d45cd93438dda9b23c97089` | `3e9ae6f6965ee1269d5574b905cf1bf559c2f44c784255b0034a71387e7719c9` |
| `xterm-addon-webgl.js` | `9ffa9ac3ff6d47d4e6216ed1972ca8e0b5336cef744f50ebdc3f67b0ed727cdb` | `3a67061bc7972fd038dac7fff6df410b879f206bc09eb18b35673eeb3c1f2de4` |
| `xterm.css` | `ba8e6985669488981ccf40c0cefe3aba80722cb6c92de7ad628b0bd717faf2b6` | same |

## Checking or updating

Download the tarball (`npm pack @xterm/xterm@5.5.0`, or its `dist.tarball` URL from
`https://registry.npmjs.org/@xterm/xterm/5.5.0`), check its SHA-512 against the
integrity above, extract it, remove the last `//# sourceMappingURL=` line from the
`.js` file, and compare. When updating, replace the files the same way and update
this note and the LICENSE files in the same change.

## Licenses

- `LICENSE-xterm`: `@xterm/xterm` (MIT)
- `LICENSE-xterm-addon-fit`: `@xterm/addon-fit` (MIT)
- `LICENSE-xterm-addon-webgl`: `@xterm/addon-webgl` (MIT)
