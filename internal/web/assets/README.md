# Vendored browser libraries

These files are embedded into the binary (`internal/web/web.go`) and served from
`/assets/` so the mirror page keeps working without internet access. They are
third-party assets only; the Go code itself still depends on nothing but the
standard library (plus `golang.org/x/text`).

| File | Library | Version | License | Source |
|------|---------|---------|---------|--------|
| `marked.min.js` | [marked](https://github.com/markedjs/marked) | 12.0.2 | MIT | `https://cdn.jsdelivr.net/npm/marked@12.0.2/marked.min.js` |
| `dompurify.min.js` | [DOMPurify](https://github.com/cure53/DOMPurify) | 3.0.11 | Apache-2.0 / MPL-2.0 | `https://cdn.jsdelivr.net/npm/dompurify@3.0.11/dist/purify.min.js` |

Both files are kept verbatim (license headers included). To upgrade, download the
new versions over these paths and note the versions here.

`marked` renders GitHub-flavored markdown (including tables) in the browser;
DOMPurify sanitizes the result before it is assigned to `innerHTML`.
