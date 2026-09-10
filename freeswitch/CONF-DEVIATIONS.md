<!-- SPDX-License-Identifier: Apache-2.0 -->

# `freeswitch/conf/` versus vanilla FreeSWITCH v1.11.3

This tree is copied over `/usr/local/freeswitch/conf` in the image. Its baseline
is upstream, exactly:

```sh
git -C <freeswitch-source> archive v1.11.3 conf/vanilla | tar -x -C <scratch>
```

Everything below is the complete difference from that. It replaced
`entrypoint.d/10-aicc.sh`, which used to apply the same changes with `sed` at
every boot against whatever a third-party image happened to unpack. The
entrypoint no longer edits configuration; it only substitutes the placeholder
values marked below from the environment.

Reasoning lives in `docs/design/01-telephony.md` §7 (diffs D1–D8 and the D6a
amendment). This file is the reviewer's index, so nobody has to diff 180 XML
files.

## Files changed

| File | Change | Why |
| --- | --- | --- |
| `autoload_configs/modules.conf.xml` | Load `mod_pgsql`, `mod_http_cache`, `mod_callcenter`, `mod_audio_stream`. Unload `mod_httapi` and `mod_av`. | `mod_pgsql` turns a `pgsql://` DSN into a connection; without it the queue engine comes up with no queues. `mod_callcenter` is the queue engine, loaded last so it reads its configuration through the Lua binding. `mod_http_cache` is what lets `record_session` write to an object store. `mod_audio_stream` is the transcription tap, loaded unconditionally so a missing module is a boot failure rather than a transcript panel that says "Connecting…" for ever. `mod_httapi` claims the same `http://` file formats `mod_http_cache` needs and only one can have them. `mod_av` is not in the image's build list, and a load line for an unbuilt module is a CRIT at every boot. |
| `autoload_configs/lua.conf.xml` | `xml-handler-script=aicc_xml.lua`, `xml-handler-bindings=directory\|configuration`. Vanilla's comments kept. | This is what makes PostgreSQL the switch's configuration: the SIP directory and `callcenter.conf` are rendered from the database instead of read from this tree. Binding needs a restart, not a `reloadxml` — mod_lua reads these when it loads and is not unloadable. |
| `autoload_configs/event_socket.conf.xml` | Port `18021` not `8021`; `listen-ip` `0.0.0.0` not `::`; `apply-inbound-acl=aicc_esl`; `stop-on-bind-error=true`. Password left at the stock `ClueCon`. | 18021 is what the project documents everywhere (`AICC_ESL_ADDR`). A compose network has no IPv6 and `mod_event_socket` answers a bind on `::` by not starting its listener at all — the switch looks healthy and nothing can connect. `stop-on-bind-error` makes that failure visible. The password and listen address are rewritten by the entrypoint from `AICC_ESL_PASSWORD`; the shipped value is the stock default on purpose, so a published image carries no credential anyone uses. |
| `autoload_configs/acl.conf.xml` | Added an `aicc_esl` list: loopback plus the three RFC1918 ranges. Vanilla's `lan` and `domains` lists untouched. | Unset, `mod_event_socket` applies `loopback.auto` and rejects the application, which lives at another address on the compose network; `localnet.auto` alone would shut `fs_cli` out of the switch's own container. No site's subnet is in the file — the port is not published, so the list is not the access control, and a real address would only document the office it was written in. |
| `autoload_configs/switch.conf.xml` | `sessions-per-second` 30 → 100; `uuid-version=7` present and uncommented; `rtp-start-port=16384`, `rtp-end-port=16584`. | 30 is a rate limit an outbound ramp exceeds, and the surplus looks from the application like calls that were never placed. v7 UUIDs sort by creation time. The RTP range is pinned because a container has to publish exactly these ports. `uuid-version` is deliberately present rather than commented: a commented parameter reads to `grep` as if it were set, which is how it went unset here once already. |
| `autoload_configs/http_cache.conf.xml` | `enable-file-formats` false → true. | Loading `mod_http_cache` is not enough. With this false the module only fetches URLs on demand and `record_session http://…` is rejected as an unknown format, so a deployment whose `aicc_recordings_dir` is an object store records nothing and reports no fault. Vanilla ships false because `mod_httapi` claims the same formats; `mod_httapi` is not loaded. |
| `sip_profiles/internal.xml` | `context` `public` → `aicc`. | The single most load-bearing change. A phone's INVITE takes the profile's context, and in `public` the stock `public_extensions` rule transfers `10xx` into the stock `default` dialplan before any aicc rule is reached — `dialplan/aicc.xml` would be installed, valid, visible to `xml_locate`, and never executed. Design 01 §7 D6a records the three things tried first that did not work. |
| `sip_profiles/internal-ipv6.xml` | `context` `public` → `aicc`. | The same profile on the other address family. A phone reaching it while sitting in `public` would have its dialled extension handed to the inbound doorway as though it were a DID. |
| `vars.xml` | `default_password` → `REPLACE_ME`. Added `aicc_lua_dsn`, `aicc_cc_dsn`, `aicc_bot_host`, `aicc_bot_port`, `aicc_recordings_dir`. `global_codec_prefs`/`outbound_codec_prefs` lose `H264,VP8`. `external_rtp_ip`/`external_sip_ip`: `stun-set` → `set` `$${local_ip_v4}`. Comments added on `domain` and `internal_auth_calls`. | Every value added is a placeholder the entrypoint rewrites (`AICC_DB_HOST`, `AICC_LUA_PASSWORD`, `AICC_DB_PASSWORD`, `AICC_BOT_HOST`, `AICC_BOT_PORT`, `AICC_RECORDINGS_DIR`, `AICC_DOMAIN`, `AICC_EXTERNAL_IP`). H264 and VP8 need `mod_av`/`mod_h26x`, which are neither built nor loaded, so every call would negotiate a video codec the switch cannot carry. STUN answers with the address the whole network is seen at from the internet, and the bot leg would be told to send its media out of the host and back — and it means the image reaching a third-party server on every boot. `internal_auth_calls` is already `true` in v1.11.3 and stays true; only a comment was added saying why it must. **`local_ip_v4` is not pinned anywhere** — a pinned one goes stale the first time DHCP moves the box, and presents as every call failing at once with nothing in the logs naming an address. |
| `dialplan/public.xml` | Removed `public_extensions` (`10xx`) and `public_conference_extensions` (`3500–3819`). | Both transfer an unauthenticated inbound call into the stock `default` context, and both sit *above* the `public/*.xml` include where this product's doorway lives — an include's position is fixed in the file, not by filename. `3500–3819` is an ordinary four-digit DID range: a deployment handed one would find its calls answered by a stock conference, with nothing in the configuration naming the rule that took them. |
| `directory/default.xml` | Removed the `sales`/`billing`/`support` groups; kept the domain, its params and the `default` group's glob. | Those groups are `type="pointer"` entries for users 1000–1014, which are deleted below; dangling pointers warn at every boot. See the deletion note for why the users went. |

## Files added (this project's, all carrying SPDX)

| File | What it is |
| --- | --- |
| `dialplan/aicc.xml` | The dialplan context every aicc-managed SIP account lives in: queue extensions `7xxx`, agent-to-agent `10xx`, and the include seam. Unchanged from what the repository already held. |
| `dialplan/public/05_aicc.xml` | The inbound doorway: every external number goes to `aicc_inbound.lua`. Numbering plans live in the database. Unchanged. |
| `sip_profiles/external/aicc_bot.xml` | The gateway that reaches the application's own SIP server for AI calls. No registration; `ping` for real OPTIONS keepalive. Unchanged. |
| `dialplan/aicc/.gitkeep`, `dialplan/aicc/README.md` | The seam `dialplan/aicc.xml` includes. A deployment's own rules — its trunk, its feature codes — go here. The directory has to exist in the image or the include has nowhere to point; the README says what belongs there and warns that a second file opening `<context name="aicc">` is silently ignored. |
| `directory/default/.gitkeep` | Keeps the (now empty) static-directory fallback directory present, since its glob is still included. |

## Files deleted from vanilla

| File(s) | Why |
| --- | --- |
| `directory/default/1000.xml` … `1019.xml` | The SIP directory is served from PostgreSQL by `aicc_xml.lua`, which renders a complete `<domain>` of its own. `1000–1019` is exactly the extension range an agent phone uses (`dialplan/aicc.xml` matches `^(10[0-9][0-9])$`), and these ship with `$${default_password}` — a stale demo account would shadow a real agent with a password of `1234`. Design 01 §7 D3 kept them as a migration fallback; the migration is done. **Second effect, load-bearing rather than incidental**: the `domains` ACL builds itself by scanning this directory for `cidr=` tags and the internal profile applies it as `apply-inbound-acl`, so with no users to scan it admits nobody without a challenge — which is what `internal_auth_calls=true` was supposed to mean all along (D6a note 3 records it not meaning that). |
| `directory/default/brian.xml`, `example.com.xml`, `skinny-example.xml` | Demo accounts for the same directory, one of them for `mod_skinny`, which is neither built nor loaded. |
| `directory/default/default.xml` | The passwordless `default` user. Its own comment says to remove it if you would rather not have one; a call centre would rather not have one. |
| `dialplan/public/00_inbound_did.xml` | Maps the demo DID `5551212` into the stock `default` context, and sorts ahead of `05_aicc.xml`. |

## Left in vanilla, and what could surprise a deployment

Reported rather than removed, because removing them is a product decision and
none of them is reachable by an ordinary call as the tree now stands.

1. **`mod_signalwire` is still loaded** (vanilla loads it). Unadopted it has no
   token, so it retries a provisioning call to SignalWire's API and logs about
   it — an outbound network connection a self-hosted deployment did not ask for.
   Nothing in this product uses it. Commenting out its `<load>` line is a
   one-line change if that is wanted.
2. **`dialplan/default.xml` and `dialplan/default/` are intact** — the stock
   demo dialplan, roughly 700 lines: conference rooms, the 5000 demo IVR, the
   pizza demo, the talking clock, voicemail, `Local_Extension`. Nothing routes
   there any more: the internal profiles are in `aicc`, and the two `public`
   rules that used to transfer into `default` are gone. It becomes reachable
   again the moment something in `dialplan/aicc/` says `transfer … XML default`,
   so do not write that.
3. **`ivr_menus/`, `lang/*/demo/`, `chatplan/`, `skinny_profiles/`** are the
   demo assets those rules referenced. Dead weight, not a route.
4. **`sip_profiles/external-ipv6.xml` is still in `public`** and still binds
   `$${local_ip_v6}`. On a compose network with no IPv6 both `-ipv6` profiles
   fail to bind and log an error at every boot. `internal-ipv6.xml` was moved to
   the `aicc` context (above) because a phone could genuinely land on it;
   `external-ipv6.xml` belongs in `public` exactly as the IPv4 external profile
   does, so it was left alone. Deleting both `-ipv6` profiles would remove the
   boot noise.
5. **`autoload_configs/` still holds ~40 config files for modules that are not
   loaded** (`amqp`, `erlang_event`, `easyroute`, `graylog2`, `hiredis`,
   `mongo`, `opal`, `osp`, `rtmp`, `shout`, `v8`, …). They are inert. Several
   contain vanilla's own example addresses and secrets — `192.168.66.6` in
   `easyroute.conf.xml`, `192.168.0.69` in `graylog2.conf.xml`, `ClueCon` as an
   Erlang cookie in `erlang_event.conf.xml` — which is worth knowing before
   anyone greps this tree for credentials and is alarmed.
6. **`autoload_configs/av.conf.xml` does not parse as standalone XML.** Upstream
   ships two sibling `<configuration>` roots in one file. FreeSWITCH accepts it
   because it is included inside `<section name="configuration">`; a plain XML
   parser does not. The file is byte-identical to vanilla and `mod_av` is
   neither built nor loaded. This is upstream's, not ours.

## Module load list

The shipped `modules.conf.xml` loads these 40 modules, and each one must be in
the image's build list (`freeswitch/modules.conf`) or the switch logs a CRIT at
boot:

```
mod_amr mod_audio_stream mod_b64 mod_callcenter mod_cdr_csv mod_commands
mod_conference mod_console mod_db mod_dialplan_asterisk mod_dialplan_xml
mod_dptools mod_enum mod_esf mod_event_socket mod_expr mod_fifo mod_fsv
mod_g723_1 mod_g729 mod_hash mod_http_cache mod_local_stream mod_logfile
mod_loopback mod_lua mod_native_file mod_opus mod_pgsql mod_png mod_rtc
mod_say_en mod_signalwire mod_sndfile mod_sofia mod_spandsp mod_tone_stream
mod_valet_parking mod_verto mod_voicemail
```

Four of them are this product's requirement rather than vanilla's:
`mod_pgsql`, `mod_http_cache`, `mod_callcenter`, `mod_audio_stream`.
`mod_audio_stream` is not a stock FreeSWITCH module and is built from source;
its build must declare `libspeexdsp` or the tap resamples at the wrong rate
(`freeswitch/assert-audio-stream.sh` checks exactly that).

Order in the file matters and is not arbitrary: `mod_pgsql` is loaded before any
endpoint so that a `pgsql://` DSN can be opened, and `mod_callcenter` is loaded
last, after `mod_lua`, because it reads its queues through the Lua XML binding —
loaded first it finds no queues and says nothing about why.

## Verifying this tree

```sh
# every XML file parses (av.conf.xml is upstream's known two-root file)
find freeswitch/conf -name '*.xml' -print0 | xargs -0 python3 -c \
  "import xml.etree.ElementTree as E,sys;[E.parse(p) for p in sys.argv[1:]]"

# the complete difference from the baseline
git -C <freeswitch-source> archive v1.11.3 conf/vanilla | tar -x -C /tmp/v
diff -rq /tmp/v/conf/vanilla freeswitch/conf

# no address or credential from anyone's deployment
grep -rniE "192\.168\.|172\.16\.|183\.241|aicc@|ClueCon" freeswitch/conf/
```

The last one has hits and should: our RFC1918 ranges in `acl.conf.xml`, the
stock `ClueCon` event-socket password, and vanilla's own examples in the inert
config files listed above. None of them is a real address or a working
credential.

## Two changes made after the tree was first assembled

**`autoload_configs/modules.conf.xml` — the load list now matches the build
list exactly.** Thirteen further modules were commented out: `mod_amr`,
`mod_b64`, `mod_conference`, `mod_db`, `mod_dialplan_asterisk`, `mod_enum`,
`mod_esf`, `mod_fsv`, `mod_png`, `mod_rtc`, `mod_signalwire`, `mod_spandsp`,
`mod_verto`. None of them is built by `freeswitch/modules.conf`, and a `<load>`
of a module that was not built is a CRIT at every boot and nothing else. Each
is reachable only from the stock demo dialplan, which nothing here routes to.
`mod_signalwire` had a second reason: it contacts SignalWire's provisioning API
at every boot, which a self-contained switch has no business doing.

The check that keeps the two lists in step:

```sh
grep -n 'load module' freeswitch/conf/autoload_configs/modules.conf.xml \
  | grep -v '<!--' | sed 's/.*module="\([a-z_0-9]*\)".*/\1/' | sort > /tmp/loaded
sed 's|.*/||' freeswitch/modules.conf | sort > /tmp/built
comm -23 /tmp/loaded /tmp/built   # loaded but not built: must be mod_audio_stream only
comm -13 /tmp/loaded /tmp/built   # built but never loaded: dead weight in the image
```

`mod_audio_stream` is the one legitimate entry on the left: it is built from
its own sources after FreeSWITCH, not through `modules.conf`.

**`sip_profiles/external/aicc_bot.xml` — inbound context `default` → `aicc`.**
Nothing arrives through this gateway today: the application never sends a call
back in, and a transfer moves the caller's own channel with `uuid_transfer`. It
was left at the stock `default` for that reason. With the stock `default`
context now unreachable by design, a call that did arrive would land nowhere,
so the parameter names the context this deployment actually routes.
