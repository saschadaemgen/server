# systemd units for carvilon

Two services, one per host. They only wrap the start of the existing
binary; no code path, no paths, no users change.

| Host | Unit | Scope | Role |
| ---- | ---- | ----- | ---- |
| RPi (`sash710`) | `carvilon-edge.service` | user systemd | `-role=edge` |
| VPS (`root`)    | `carvilon-cloud.service` | system systemd | `-role=cloud` |

## Files in this directory

```
carvilon-edge.service        RPi user unit (template)
carvilon-edge.env.example    RPi EnvironmentFile template
carvilon-cloud.service       VPS system unit (template)
carvilon-cloud.env.example   VPS EnvironmentFile template
README.md                    this file
```

The real `*.env` files (with secrets) are gitignored. Only the
`*.env.example` templates are committed. The `<...>` placeholders
(master key, HMAC key, the VPS / RPi IPs) are filled in by hand on the
host - never commit real values.

## RPi: carvilon-edge (user service)

The edge runs as the normal `sash710` user, not root, so it uses
*user* systemd. The unit uses the `%h` specifier, so it resolves to
that user's home (`/home/sash710`) without hardcoding the name.

```
# 1. Deploy the binary as before (scp into ~/carvilon-server/bin/).

# 2. Env file: copy the template, fill it, lock it down.
mkdir -p ~/.config/systemd/user
cp deploy/systemd/carvilon-edge.env.example ~/.config/systemd/user/carvilon-edge.env
nano ~/.config/systemd/user/carvilon-edge.env      # fill <...>, set the real IPs
chmod 0600 ~/.config/systemd/user/carvilon-edge.env

# 3. Unit file.
cp deploy/systemd/carvilon-edge.service ~/.config/systemd/user/carvilon-edge.service

# 4. Reload + enable + start.
systemctl --user daemon-reload
systemctl --user enable --now carvilon-edge.service

# 5. Verify.
journalctl --user -u carvilon-edge.service -f
```

Lingering (Sascha's hand, once): a user service stops when the last
SSH session of the user ends, unless lingering is enabled. To keep the
edge running across logouts and at boot:

```
sudo loginctl enable-linger sash710
```

## VPS: carvilon-cloud (system service)

The cloud runs as root (Sascha convention), so it uses *system*
systemd. It currently runs via `nohup` - stop that process first
(`kill <pid>`), then:

```
# 1. Deploy the binary as before (scp into /root/carvilon/).

# 2. Env file: copy the template, fill it, lock it down.
cp deploy/systemd/carvilon-cloud.env.example /etc/systemd/system/carvilon-cloud.env
nano /etc/systemd/system/carvilon-cloud.env        # fill <...>
chmod 0600 /etc/systemd/system/carvilon-cloud.env

# 3. Unit file.
cp deploy/systemd/carvilon-cloud.service /etc/systemd/system/carvilon-cloud.service

# 4. Reload + enable + start.
systemctl daemon-reload
systemctl enable --now carvilon-cloud.service

# 5. Verify.
journalctl -u carvilon-cloud.service -f
```

The cloud env sets `CARVILON_SIDECHANNEL_INTERNAL_ADDR=127.0.0.1:8445`,
which activates the interim request-publish hook on loopback for the
stream chat. Remove that line to keep the hook silent.

## Notes

- No install automation (no Ansible, no script). Run these once by hand.
- Both `.env` files must be `0600`; they hold the master/HMAC keys.
- FCM doorbell push (edge only, optional): set
  `CARVILON_FCM_SERVICE_ACCOUNT_JSON` + `CARVILON_FCM_PROJECT_ID`
  together in the edge `.env` to enable it (commented placeholders in
  `carvilon-edge.env.example`). Setting exactly one is a config error;
  both empty leaves FCM disabled and the edge starts normally. The
  service-account JSON is a secret - keep it `0600` and out of the repo.
- In-process stream server (commercial `carvilon_stream` build, edge
  only): set `CARVILON_STREAM_NVR_HOST` / `CARVILON_STREAM_API_KEY` /
  `CARVILON_STREAM_DB_PATH` / `CARVILON_STREAM_ADDR` (commented
  placeholders in `carvilon-edge.env.example`); the base URL is reused
  from `UNIFIX_STREAM_BACKEND_URL`. Incomplete config skips the
  in-process server and the edge keeps running. `CARVILON_STREAM_API_KEY`
  (the Protect X-API-KEY) is a secret - this is why the edge `.env` is
  `0600`. The public build ignores all `CARVILON_STREAM_*`.
- Both units restart on failure (`Restart=on-failure`, `RestartSec=5s`).
- Hardening directives (ProtectSystem, NoNewPrivileges, ...) are
  intentionally left out for now to avoid disturbing the existing
  paths/users; add them in a later pass if wanted.
