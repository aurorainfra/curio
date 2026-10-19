# Web UI authentication

Everything served on the GUI address (`Subsystems.GuiAddress`) requires a login:
- pages and static files;
- the webrpc API and its websockets;
- the config editor;
- the SQL console;
- pprof.

Only two things are exempt:
- CORS preflight requests, which carry no credentials and run no endpoint;
- `/mcp`, which has its own bearer tokens (`Aurora.MCP.Tokens`).

**With no enabled user configured, every request is refused.**

## Setup

Use `curio config manage-users [layer]` (default layer: `base`). It talks to the database directly, so it works while the UI is locked:

```sh
curio config manage-users                    # interactive: add, change password, disable, enable, remove
curio config manage-users --add ops          # prompts for the password twice
curio config manage-users --passwd ops
curio config manage-users --disable ops      # or --enable / --remove
curio config manage-users --list
echo "$PW" | curio config manage-users --add ci --password-stdin
```

The command:
- changes only the `[[Aurora.WebUI.Users]]` entries of the layer and keeps the rest of its text as it was;
- saves the previous version to the layer history, and refuses to write if the layer changed meanwhile;
- hashes passwords with bcrypt (cost 12, at least 12 characters).

Users in a layer replace the users of the layers below it on nodes running that layer.

To edit by hand instead, hash a password with `htpasswd -nbBC 12 "" 'a long password' | cut -d: -f2` or `go run ./aurora/webauth/cmd/hashpw`, then add it to a layer:

```toml
[Aurora.WebUI]
  SessionTTL = "12h0m0s"

[[Aurora.WebUI.Users]]
  Name = "ops"
  PasswordBcrypt = "$2a$12$..."
```

Users can be added, re-passworded or disabled without a restart. Changing a user's password or disabling them ends their sessions.

## How it works

- **Login.** Browsers get the standard HTTP Basic prompt. After the first successful login they carry a signed session cookie (`HttpOnly`, `SameSite=Strict`, valid for `SessionTTL`), which also authenticates websocket connections. Scripts can send Basic credentials on every request; verified credentials are cached for 5 minutes, so bcrypt doesn't run on every call.
- **Cross-origin protection.** Unsafe requests (anything except GET/HEAD) and websocket upgrades are refused when their `Origin` is neither the UI itself nor listed in `HTTP.CORSOrigins`.
- **Brute force.** 10 failed logins from one IP within a minute lock that IP out for a minute. Logins and failures are logged by the `aurora/webauth` logger.
- **Restarts.** Session cookies are signed with a per-process key, so a restart ends all sessions. Browsers that cached the Basic credentials log back in transparently.
- **No TLS.** Like the rest of the GUI, this is plain HTTP unless a TLS-terminating proxy sits in front. Credentials then cross the network in cleartext, so keep the port on operator networks or put it behind TLS.
