# Sales workspace preview deployment

This guide deploys the **forked** sales workspace (branch `feat/sales-workbench`), not the original upstream release. The preview can be used for browser-based evaluation before the user supplies customer records, provider credentials or a carrier trunk.

## Current deployment status

- GitHub code: fork exists, branch contains the sales workspace.
- Runtime: requires a Docker host. GitHub itself is source control, not a running instance of this application.
- Real calls: **disabled by default** in the private preview. Do not import real customers until the organization has approved data and outbound compliance policies.
- Tests and CI: verify the fork's Actions results before promoting or exposing the instance.

## Host requirements

A Linux host with **at least 4 vCPU and 4 GiB RAM** (8 GiB is preferable when building the Go app and frontend), Docker Engine >=24, Docker Compose >=2.24.4, Git, OpenSSL and access to the upstream FreeSWITCH image registry. Disk requirements depend on call recording retention; allocate at least 30 GiB for a test environment and monitor free space.

Do not deploy the sample credentials in `deploy/.env.example` to a public host. Do not expose PostgreSQL, the FreeSWITCH event socket or RTP to the Internet. Keep both web and SIP ports loopback-only during evaluation.

## 1. Clone your own development branch

```sh
git clone --branch feat/sales-workbench \
  https://github.com/siyingchao007-maker/ai-native-callcenter.git
cd ai-native-callcenter
```

Do **not** use the one-line installer or `AICC_IMAGE=rasonyang/ai-native-callcenter:<tag>` here: these run the original author's published web application, which does not include the sales workspace. The helper below compiles `aicc:local` from **this checkout**.

## 2. Launch a private demo (no AI provider key, no public carrier)

Choose the IP **actually owned by this server** on its LAN. Do not use the public NAT address unless it is bound on the server itself. The first argument configures FreeSWITCH signaling, even though phone ports stay private for this demo.

```sh
bash scripts/sales-preview-up.sh 192.168.1.20
```

Replace `192.168.1.20` with your host-owned LAN address.

The launcher:
- exits instead of replacing any existing `deploy/.env`;
- generates passwords for the database, event socket, Lua and demo accounts;
- binds the browser site and SIP endpoints to `127.0.0.1`;
- uses `AICC_PROVIDER=none`, leaves `AICC_OUTBOUND_ENDPOINT` unset and loads only the upstream **synthetic** demo data;
- builds the modified Go/React application from this branch and starts PostgreSQL, FreeSWITCH and the application through the existing Compose stack.

This still requires working Docker/images/build dependencies on the host. A failed pull, image build or unhealthy container must be diagnosed from its logs; running the command is not proof of success.

## 3. Access the browser securely

On the host itself: `http://127.0.0.1:8080`.

From your own computer, use an SSH tunnel (replace account and host; keep the tunnel open):

```sh
ssh -L 8080:127.0.0.1:8080 YOUR_USER@YOUR_SERVER
```

Open `http://127.0.0.1:8080` in the browser on that computer.

The demo admin username is `admin`. The password is the locally generated `AICC_SEED_PASSWORD` in `deploy/.env`. **Do not share the password, private key, or `.env` in chat or commit it to Git.**

After login the `/admin` page is the sales workspace. You can inspect:
- the synthetic campaigns and records;
- the editable AI flows at `/admin/bots`;
- the new management follow-up list at `/admin/callbacks`;
- the call ledger and reports;
- the campaign import flow, using test-only CSV/Excel data.

With `AICC_PROVIDER=none` the AI cannot converse, and with an empty `AICC_OUTBOUND_ENDPOINT` real outbound origination is intentionally refused. This preview proves the **browser workflow**, not production calling.

## 4. Verify the running stack

```sh
cd deploy
docker compose ps -a
docker compose logs --tail=100 aicc
curl -fsSI http://127.0.0.1:8080/ || true
```

Check the browser login route and `/admin` from the SSH tunnel. Confirm that `postgres` and `freeswitch` are healthy, the `aicc` service is running, and `lua-role` exited successfully. If something fails, follow `deploy/README.md` and `deploy/production-checklist.md`.

To stop the test stack without removing the demo database:

```sh
docker compose down
```

Do not use `down -v` unless you intentionally want to delete the local demo volumes.

## 5. Build and unit-test gate (before promotion)

On a development machine with Node.js 24 and npm 11, from the repository root:

```sh
cd web
npm ci
npm run lint
npm run test
npm run build
```

For the backend, follow the version in `go.mod` and run `go build ./...` and `go test -race ./...` from the project root. The database-backed tests need the test PostgreSQL specified in `AGENTS.md`. The repo's GitHub Actions `ci` and `web-layout` workflows provide the authoritative automated checks; forks may require explicitly enabling Actions.

## 6. Production readiness: later, after preview approval

- Supply a host/domain and controlled HTTPS access; a test GitHub fork is not a hosted web URL.
- Configure a provider key via environment configuration and verify one bilingual **simulated** call first.
- Obtain a legitimately provisioned SIP/carrier route and set `AICC_OUTBOUND_ENDPOINT` only after dialing and consent controls are verified.
- Before adding real records, disable demo seeding in a **fresh** production database (`AICC_SEED=`) and set strong separate secrets.
- Configure backup, roles, recording retention, access control, local privacy rules, DNC/opt-out handling and time windows.
- Do not expose default-port unencrypted HTTP or SIP to the public network. A reverse proxy/SBC and TLS are required where appropriate.
- Validate the system for 1 concurrent call first, then increase up to the team's approved capacity.

## Existing components reused

The stack in `deploy/docker-compose.yml` already contains PostgreSQL, FreeSWITCH, the Go API/embedded React UI, call records, AI campaigns and synthetic demo data. This deployment plan deliberately adds **no** second backend or frontend hosting framework.

## Current limitations

The dashboard's daily call totals come from the existing platform-wide ledger, **not a campaign-only sales conversion report**. The management callback queue displays currently open items returned by the existing list API and is read-only; agents handle callbacks from their own workspace. Scheduled reminders, verified lead conversion KPIs, and a commercial outbound carrier connection remain separate development/integration work.

This document is a deployment guide and does not claim a cloud instance has already been created.
