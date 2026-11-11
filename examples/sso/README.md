# examples/sso

OIDC sign-in (`--sso-*`, `gwag login --sso`) against a real IdP: Dex in
Docker, bound to 127.0.0.1.

```
./run.sh   # Dex :28556, gateway :28500; open http://127.0.0.1:28500
./e2e.sh   # headless Chrome (CDP) + the CLI; screenshots in .run/
```

Users are in `dex.yaml` (test fixtures): `admin@example.test` is admin
(email rule), `dev@example.test` is dev (domain rule), `stranger@other.test`
matches no rule and is denied. Dex static passwords carry no groups, so
there are no `group:` rules here.

`e2e.sh` checks: sign-in through the Dex form, whoami, the role chip, an
admin mutation (REST and GraphQL) same-origin, the same refused from
another origin, logout, the dev role (admin op refused, dev op allowed),
the denied page, then `gwag login --sso --no-browser` driven through the
browser, `gwag peer list`, and `gwag logout` ending the bearer. Needs
docker, google-chrome, node 22+; skips without docker or Chrome.
