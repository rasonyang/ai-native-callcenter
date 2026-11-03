# Deploy the private sales preview on DigitalOcean

This guide gives the owner a concrete, **one-time** cloud purchase and startup path. No customer list, phone provider key, cloud token or SSH private key is required in the repository.

## Selected server

- Provider: DigitalOcean Droplet, datacenter **Singapore (SGP1)**.
- Image: **Ubuntu 24.04 LTS, x86_64**.
- Plan: **Basic / Regular**, **4 vCPU, 8 GiB RAM, 160 GiB SSD**.
- Reference rate: **USD 48/month** for the shared-CPU Basic plan, excluding optional add-ons and taxes. Verify the price in the provider console.
- Authentication: SSH public key, not password authentication.
- Tag: `sales-workbench-preview`.
- Firewall: inbound **SSH/TCP 22 only**, preferably from the owner's IP; allow normal outbound HTTPS/package installs.
- No customer data. No external telephone trunks. No API key in startup user data.

## Create the Droplet

1. Go to <https://cloud.digitalocean.com/droplets/new> and sign in.
2. Pick the **Singapore** region and **Ubuntu 24.04 (x64)** image.
3. Select the **Basic regular** plan with **4 vCPU / 8 GB**.
4. Select **SSH keys** under authentication; register your public key if you have not already done so.
5. Under **Additional Options**, enable **Startup scripts**. Open [scripts/digitalocean-user-data.sh](../../scripts/digitalocean-user-data.sh), copy the entire script text and paste it into the user-data box. These scripts are executed as root on the new server's **first boot only**; review them before enabling. The script installs Docker, clones this fork's `feat/sales-workbench` branch, generates independent demo passwords, builds the application and tries the local web endpoint.
6. Add the tag `sales-workbench-preview`. Create the Droplet. The provider will display a public IPv4 address; this public **address only** is okay to share with the developer when requesting help.
7. In **Networking > Firewalls**, set an SSH-only Cloud Firewall and apply it to the tag. Inbound: TCP 22, restrict to your IP if possible. **Do not** open 8080, 5060, 5066 or the RTP ports to the public Internet for a browser-only preview.

Cloud-init output is in `/var/log/cloud-init-output.log` and the installer also writes to `/var/log/aicc-sales-bootstrap.log`. Installing Docker and building the application needs outbound Internet access. For safety, the app's HTTP and SIP host ports are bound to `127.0.0.1` even though the VM itself has a public IP.

## Check startup

Connect with your SSH key:

```sh
ssh root@YOUR_SERVER_IP
```

If a non-root sudo user has been created, use that identity instead. On the server:

```sh
sudo cloud-init status --wait
sudo tail -n 60 /var/log/aicc-sales-bootstrap.log
cd /opt/ai-native-callcenter/deploy
sudo docker compose ps -a
curl -I http://127.0.0.1:8080/login
```

A complete startup requires an answering web endpoint and healthy dependent containers. A provisioning script having been pasted or executed is **not** proof that deployment worked.

To troubleshoot while **not** exposing passwords:

```sh
sudo docker compose logs --tail=100 aicc
sudo docker compose logs --tail=50 freeswitch
```

The demo administrator login is `admin`. Retrieve its password **privately** on the server using:

```sh
sudo grep '^AICC_SEED_PASSWORD=' /opt/ai-native-callcenter/deploy/.env
```

Do not paste the output of that command into any chat or issue.

## Open the site from a laptop

Keep the original SSH session open if it is still running. On your **own computer** open a separate terminal:

```sh
ssh -N -L 8080:127.0.0.1:8080 root@YOUR_SERVER_IP
```

Browse to <http://127.0.0.1:8080/login>. Log in using the private demo password. This SSH tunnel makes the site accessible without publishing its unsecured test HTTP endpoint.

If browsing from a phone is required later, set up a properly authenticated and TLS-protected private access path (such as a VPN or Cloudflare Access). Do **not** change `HTTP_BIND` to `0.0.0.0` merely to make the page reachable from a phone.

## Expected behavior

- Login, navigate Chinese UI, open /admin (Sales Workspace).
- View synthetic call history, configured flows, campaign management and callbacks.
- The current sales workspace uses existing platform-wide call report fields; numbers are demo data, not your team's results.
- Real AI conversation is disabled: `AICC_PROVIDER=none`.
- Real outbound origination is disabled: `AICC_OUTBOUND_ENDPOINT=`.
- No customer records or carrier details need to be imported before demo evaluation.

## Next milestone

The cloud server purchase is the only account-controlled step. Once the Droplet has actually been created, pass along **only the public IP** (no root password, no SSH private key, no private environment file). Then review the nonsecret provisioning state and verify the browser login before attempting any actual telephone integration.

The full forked-stack setup reference is [DEPLOYMENT.md](DEPLOYMENT.md). Upstream runtime documentation is [deploy/README.md](../../deploy/README.md). Production hardening is [deploy/production-checklist.md](../../deploy/production-checklist.md).
