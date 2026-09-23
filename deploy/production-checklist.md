# Production checklist

Work through this before the stack described in [the deployment guide](README.md)
faces anyone.

- [ ] Change every password: PostgreSQL, `aicc_lua`, ESL, and every seeded
      account. The defaults are published in the deployment guide.
- [ ] Set `AICC_SEED=` (empty) in `.env` so no demo data lands on a host others
      can reach. If the database already has it, run once with
      `AICC_SEED=fresh` to remove exactly what the seed created.
- [ ] Put TLS in front; this stack does not provide it. Proxy all of `/` (API,
      event stream and interface share one origin) and set
      `AICC_SECURE_COOKIES=true`, or the session cookie is never sent back.
      Turn response buffering **off** and set a read timeout longer than a quiet
      stream: with buffering, Server-Sent Events arrive in batches; with a short
      timeout, browsers reconnect endlessly.
- [ ] Set `SIP_WSS_URL` to a `wss://` endpoint once the interface is on TLS. An
      `https://` page cannot open a plain `ws://` socket, so the agent's phone
      would never register. The default is `ws://`.
- [ ] Keep `AICC_METRICS_ADDR` reachable only inside the stack. It has no
      authentication; the compose file publishes no port for it.
- [ ] Firewall SIP and the RTP range so only the phones that need them can
      reach them. The AI leg's SIP and RTP are not published and need no rule.
- [ ] Name every API key after the integration that holds it and give it only
      the scopes it needs, so a leaked key can be revoked on its own.
- [ ] Keep provider keys in the environment, never in the database or a flow.
- [ ] Raise the provider's concurrency quota to match your traffic. It is an
      external limit that local capacity cannot replace.
