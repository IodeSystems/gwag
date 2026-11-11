# examples/sso

OIDC sign-in (`--sso-*`, `gwag login --sso`) against a real IdP in
Docker, bound to 127.0.0.1: Dex (default) or Keycloak.

```
./run.sh                # Dex :28556, gateway :28500; open http://127.0.0.1:28500
IDP=keycloak ./run.sh   # Keycloak :28557 (about 30 s to start)
./e2e.sh                # headless Chrome (CDP) + the CLI; screenshots in .run/
IDP=keycloak ./e2e.sh
```

**Dex** (`dex.yaml`, test fixtures): `admin@example.test` is admin
(email rule), `dev@example.test` is dev (domain rule),
`stranger@other.test` matches no rule and is denied. Dex static passwords
carry no groups, so there are no `group:` rules here.

`e2e.sh` checks: sign-in through the Dex form, whoami, the role chip, an
admin mutation (REST and GraphQL) same-origin, the same refused from
another origin, logout, the dev role (admin op refused, dev op allowed),
the denied page, then `gwag login --sso --no-browser` driven through the
browser, `gwag peer list`, and `gwag logout` ending the bearer.

**Keycloak** (`keycloak-realm.json`, imported at start; the passwords in
it are fixtures): realm `gwag`, confidential client `gwag`, a Group
Membership mapper `groups` (full path off, ID token and userinfo), groups
`gwag-admins` and `gwag-devs`. Rules are `group:gwag-admins=admin` then
`group:gwag-devs=dev`, no email or domain rules. Users `grp-admin`,
`grp-dev`, `grp-both` (both groups: admin, first rule wins), `grp-none`
(denied), `grp-demote` (both groups), all `@corp.test`.
`IDP=keycloak ./e2e.sh` checks each role, then removes `grp-demote` from
`gwag-admins` through Keycloak's admin REST API: the open session keeps
`admin` (groups are in the cookie); after sign-out and sign-in again the
role is `dev`. `gwag login --sso` as `grp-admin` and `gwag peer list`
follow. A second phase restarts the gateway with
`--sso-groups-claim roles_groups` (a second mapper, full path on) and
rules on `/gwag-admins` paths, dev rule first.

Needs docker, google-chrome, node 22+; skips without docker or Chrome.
