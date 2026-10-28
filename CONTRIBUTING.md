# Contributing

## Branches

| Branch | What is in it | Node image |
|---|---|---|
| `main` | stable: only what was tested on `dev` | `ghcr.io/admiralcore/echofog-node:latest` |
| `dev` | new work, tested here first | `ghcr.io/admiralcore/echofog-node:dev` |
| anything else | long or risky work (`feat/...`, `exp/...`) | none |

- Open pull requests against `dev`, and push day-to-day work there.
- `dev` goes into `main` once it has run on a test server; nothing is pushed to `main` directly.
- A fix that cannot wait is a branch from `main`, merged into `main` and then into `dev`.
- A release is a tag `vX.Y.Z` on `main`: it builds the images `:X.Y.Z` and `:X.Y` and a draft GitHub
  release with the desktop app and the node.

Every push and pull request runs the tests (`.github/workflows/test.yml`); pushes to `main` and `dev`
also build the node image and the binaries.

## Commits

Say what changed and why, in plain words, in the first line; details in the body if needed.
