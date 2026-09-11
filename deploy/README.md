# Deploying

One executable, one database, one switch.

The application is a single binary with the web interface inside it. It needs
PostgreSQL for state and FreeSWITCH for telephony, and nothing else — no
message broker, no cache, no object store unless you want one.

For a machine to try it on, use [the demo stack](demo/README.md) instead;
everything below is about a deployment that carries real calls.

## What it runs on

| | |
|---|---|
| CPU / memory | Not yet characterised. Nothing here has been benchmarked, so size from your own traffic, watch `AICC_METRICS_ADDR`, and grow from what you see. |
| PostgreSQL | 18. The application migrates its own schema at startup. |
| FreeSWITCH | This project's own image, `rasonyang/freeswitch-aicc` (`linux/amd64`, `linux/arm64`), or a native install of 1.11.3 or newer with `mod_lua`, `mod_pgsql` and `mod_callcenter`. |
| Network | The AI leg terminates its own SIP and RTP: one UDP port for signalling (6060) and a range for media (40000–40999 by default), reachable from the switch. |
| Storage | Recordings on a filesystem, or any S3-compatible endpoint. |

FreeSWITCH may share the machine or not. If it does, keep the two RTP ranges
apart — the defaults already are (FreeSWITCH 16384–32768, the bot 40000–40999).

## Installing

```sh
# 1. The database.
createdb aicc
createdb aicc_fs          # mod_callcenter's own tables; see below

# 2. The application.
tar -xzf aicc_v0.1.0_linux_amd64.tar.gz && cd aicc_v0.1.0_linux_amd64
cp .env.example .env      # then uncomment what this deployment changes
./aicc                    # migrates, then serves

# 3. The first administrator, against the now-migrated database.
./aicc useradd -username admin -password '…' -role ADMIN
```

Or as a container:

```sh
docker run -d --name aicc \
    -e AICC_DATABASE_URL='postgres://aicc:…@db:5432/aicc' \
    -e AICC_ESL_ADDR='fs:18021' -e AICC_ESL_PASSWORD='…' \
    -e AICC_SWITCH_DOMAIN='pbx.example.com' \
    -e OPENAI_API_KEY='…' \
    -p 8080:8080 -p 6060:6060/udp -p 40000-40999:40000-40999/udp \
    ghcr.io/rasonyang/ai-native-callcenter:v0.1.0
```

Only exact release tags are published — there is no `latest`, so a deployment
names the build it runs and an image pull cannot quietly change under it.

Then the switch. [`freeswitch/README.md`](../freeswitch/README.md) has both
procedures; whichever you take, after it **adding an extension, a queue or a
number is a database change and FreeSWITCH is never edited again**.

* **The image.** `rasonyang/freeswitch-aicc:v1.11.3` is FreeSWITCH built from
  source with this project's configuration tree, its Lua scripts and
  `mod_audio_stream` inside it. Nothing site-specific is baked in: the database
  DSNs, the addresses, the passwords and the trunk all arrive as environment
  variables at container start.
  [`freeswitch/DOCKERHUB.md`](../freeswitch/DOCKERHUB.md) is that contract in
  full, and `deploy/demo/docker-compose.yml` is a worked example of it.
  `AICC_LUA_DSN` and `AICC_CC_DSN` are required — without them the switch
  refuses to start, because one that started would look healthy and tell every
  agent phone that its extension does not exist.
* **A native install.** Build FreeSWITCH yourself with at least the modules in
  `freeswitch/modules.conf`, copy `freeswitch/conf/` and `freeswitch/scripts/`
  over the switch's own, and fill in the placeholders the image's entrypoint
  would have substituted. The README lists them.

Two details are easy to skip on either path and both are fatal:

* **mod_callcenter needs a database of its own.** It creates unqualified
  `agents`/`tiers`/`members` tables, which collide with the application's. Give
  it `aicc_fs`.
* **Binding the XML handler takes a restart, not a reload.** `reload mod_lua`
  answers "Module is not unloadable" and leaves the binding silently inactive.
  The image restarts anyway; a native install has to be told.

## Configuring

**[`.env.example`](../.env.example) is the registry.** Every setting the server
reads is in it, commented out, showing the value already in use. Copy it,
uncomment what changes, leave the rest — a default that improves in a later
release then reaches this deployment too.

Two of its semantics surprise people:

* An empty value means *unset*, so the default wins. A setting whose default is
  non-empty cannot be blanked; give it another value instead.
* There is no inline-comment syntax. Everything after the first `=` is the
  value.

The settings a deployment almost always changes:

| | |
|---|---|
| `AICC_DATABASE_URL` | Where PostgreSQL is |
| `AICC_ESL_ADDR`, `AICC_ESL_PASSWORD` | Where the switch is. Change the password. |
| `AICC_SWITCH_DOMAIN` | Must equal FreeSWITCH's own `$${domain}` — queue names are rendered with it on both sides and matched literally |
| `AICC_PROVIDER` + its key | `qwen` inside mainland China, `openai` elsewhere. One provider answers every call; see [the provider notes](../docs/provider-extension.md). |
| `AICC_RECORDING_DIR`, `AICC_RECORDING_BACKEND` | Recording is off until the spool directory is set |
| `AICC_SECURE_COOKIES` | `true` wherever the interface is served over HTTPS, or the session cookie is never sent back |
| `AICC_METRICS_ADDR` | Loopback by default. That listener has no authentication of its own. |
| — | There is no API-key setting. Keys are issued through `POST /api-keys`, each with its own name and scopes, and revoked one at a time. The first one is issued with the administrator's session, and a session cookie must carry `X-AICC-Csrf` on anything that writes — any value; its presence is the check — or the request is refused `FORBIDDEN: missing X-AICC-Csrf header`. A `Authorization: Bearer` key never needs it. |

## In front of it

Terminate TLS at a reverse proxy and give it the whole of `/`: the API, the
event stream and the interface are one origin by design.

The event stream is Server-Sent Events, which needs two things from a proxy
that defaults the other way: **response buffering off**, and a read timeout
longer than a quiet stream. With buffering on, events arrive in batches minutes
late; with a short timeout, browsers reconnect in a loop.

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;          # SSE
    proxy_read_timeout 1h;        # SSE
}
```

The browser softphone registers to FreeSWITCH over WSS, not through this proxy.

## Recordings

`AICC_RECORDING_DIR` is the spool the switch writes into; nothing is ingested
until it is set. Both backends key on
`recordings/YYYY/MM/DD/<call_id>.wav` — stereo, caller left, bot or agent
right.

* **`FS`** serves files straight from that directory, which assumes the
  application can read what the switch wrote. Same host, or a shared mount; if
  the two run as different users, the application's user needs to traverse the
  directories the switch creates.

  That is not automatic, and the way it fails is quiet: the switch creates
  `YYYY/MM/DD` as `drwxr-x---` owned by itself — uid 999 in this project's
  image — so the recording is written, the call is fine, and ingestion logs
  `no recording ingested … permission denied` while the CDR says there is
  audio. Grant the traversal once, with inheritance, so it also covers the
  directories tomorrow's calls create:

  ```sh
  setfacl -R    -m u:aicc:rX /var/lib/aicc/recordings   # what is already there
  setfacl -R -d -m u:aicc:rX /var/lib/aicc/recordings   # what the switch creates next
  ```

  A shared group works too, as long as it is inherited — the mode the switch
  writes leaves nothing for "other".
* **`S3`** uploads on hangup and clears the spool, and playback redirects to a
  presigned URL. Any S3-compatible endpoint (`AICC_S3_*`); verified against
  SeaweedFS as well as AWS.

Retention is a setting in the product, not a configuration file: a daily job
deletes objects older than `retentionDays` and stamps the row. Zero keeps them
forever.

## Upgrading

Migrations run at startup and are forward-only. Stop the old binary, start the
new one; a single-instance advisory lock means two of them cannot race.

The switch's configuration is versioned with the schema on purpose: the Lua
scripts read the `luacc.*` views and nothing else, and any migration touching
the base tables re-asserts those view shapes. An upgrade therefore does not
normally touch FreeSWITCH — but the release notes say when it does.

## Before it faces anyone

- [ ] `AICC_SEED` unset. The demo dataset has published passwords.
- [ ] Every default password changed: PostgreSQL, `aicc_lua`, ESL, and the
      bootstrap administrator.
- [ ] `AICC_SECURE_COOKIES=true` and TLS in front.
- [ ] `AICC_METRICS_ADDR` on loopback or behind the proxy's authentication.
- [ ] The event socket unreachable from outside the host — ESL is a shell on
      the switch, and its ACL is the only thing standing in front of it. On
      `--network host` that ACL allows the whole of RFC1918 as shipped; bind
      the listener to loopback instead (`FS_ESL_LISTEN_IP`).
- [ ] The SIP UAS reachable from the switch and nowhere else. `AICC_BOT_SIP_HOST`
      settles the signalling port; the RTP range does not follow it — it binds
      every interface whatever that is set to — so the media ports are closed
      by a host firewall or by nothing.
- [ ] Every issued API key has a name saying which integration holds it, and
      only the scopes that integration needs. A key is a managed credential:
      a leaked one is revoked on its own, which is exactly what the shared
      secret it replaced could never do.
- [ ] Provider keys in the environment, never in the database, never in a flow.
- [ ] Provider concurrency quota raised to match the traffic. It is an external
      limit and no amount of local capacity substitutes for it.
