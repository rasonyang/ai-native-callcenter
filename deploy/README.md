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

Nothing below is installed for you. A clean Ubuntu 24.04 host has neither
PostgreSQL nor its client tools nor a container runtime, so the first command
of every procedure here fails with `command not found` until they are there.
The image path needs a container runtime in any case — the switch ships as one.

```sh
sudo apt-get install -y docker.io postgresql-client acl
```

`acl` is for `setfacl`, which the recording section below depends on;
`postgresql-client` is `psql` and `createdb`, needed even when the server
itself is a container, because the switch's read-only role is created with
them.

```sh
# 1. The database. PostgreSQL 18, wherever it lives. As a container:
docker run -d --name aicc-db --restart unless-stopped \
    -e POSTGRES_USER=aicc -e POSTGRES_PASSWORD='…' -e POSTGRES_DB=aicc \
    -p 127.0.0.1:5432:5432 -v aicc-pgdata:/var/lib/postgresql postgres:18
createdb aicc_fs          # mod_callcenter's own tables; see below

# 2. The application.
tar -xzf aicc_v0.1.0_linux_amd64.tar.gz && cd aicc_v0.1.0_linux_amd64
cp .env.example .env      # then uncomment what this deployment changes
./aicc                    # migrates, then serves

# 3. The first administrator, against the now-migrated database.
./aicc useradd -username admin -password '…' -role ADMIN
```

The volume goes at `/var/lib/postgresql`, not at `/var/lib/postgresql/data`.
The 18 image puts its cluster in a subdirectory of the former so that
`pg_upgrade --link` has both versions inside one mount point, and a container
given the old path exits at every start with `This is usually the result of
upgrading the Docker image without upgrading the underlying database`.

Publish the port on loopback. The database has no business being reachable
from the network when the application shares its host.

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

### As a service

`./aicc` in step 2 is the foreground form, which is what you want while you
are still reading its output. A deployment that survives a reboot needs the
process supervised, and the release tarball carries no unit file, so write one:

```ini
# /etc/systemd/system/aicc.service
[Unit]
Description=aicc — AI-native call center
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=aicc
Group=aicc
WorkingDirectory=/opt/aicc
ExecStart=/opt/aicc/aicc
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

`.env` is read from the working directory and holds the database password and
the provider key, so it belongs to that user and to nobody else
(`chown aicc:aicc /opt/aicc/.env && chmod 600 /opt/aicc/.env`). Give the
containers `--restart unless-stopped` for the same reason: after a reboot the
switch and the database have to come back on their own, and the unit above
starts before Docker has finished starting them — `Restart=on-failure` is what
closes that gap, so expect the service to be `activating` for a few seconds
after boot before it reports `active`.

Until the switch is up and the application logs `esl connected`, creating an
account is refused: provisioning a SIP extension is a change the switch has to
be told about, and the request times out as `STORAGE_DOWN` rather than leaving
an extension that exists in one place only. Bring the switch up first, or retry.


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

## Answering a call

A migrated database holds no numbers and no conversations, so the deployment
you now have answers nothing. Both are API objects, and the order matters:

```sh
# 1. A flow. Creating stores a draft.
curl -X POST …/api/v1/flows -d '{"slug":"…","name":"…","spec":{…}}'

# 2. Publishing is what a call runs. A number pointing at an unpublished
#    flow reaches no bot.
curl -X POST …/api/v1/flows/<flowId>/publish

# 3. The number, pointing at that flow.
curl -X POST …/api/v1/dids -d '{"number":"95001","language":"en","flowId":"<flowId>","allowInbound":true}'
```

An inbound number requires a flow — the contract refuses one without it — and
the number's `language` is what the bot greets in, not a property of the flow.
`spec` is the v2 flow DSL, validated by the server's loader: what it rejects
with 422 is exactly what could not have run. `internal/seed/flows/` in the
repository holds working examples, and `AICC_SEED=demo` installs a complete
set on a machine that is only being tried out.

FreeSWITCH needs to be told nothing about any of this. The inbound rule hands
every external number to the Lua entry point, which asks the database which
flow the number carries.

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

  `setfacl` is in the `acl` package, which a minimal server install does not
  have. Without it the command answers `setfacl: command not found` and the
  symptom above stays exactly as it was.

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
