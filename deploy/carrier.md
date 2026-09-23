# Connecting your own carrier

This applies to the compose stack in [the deployment guide](README.md). Paths
are relative to `deploy/`.

Two directories are mounted into the switch:

- `freeswitch/dialplan/`: rules of the `aicc` context.
- `freeswitch/directory/`: SIP users the switch knows outside the database.

They currently hold a simulated public network, which is what lets a softphone
act as a customer. For a real deployment, replace them with your own trunk
files and mount your gateway into `conf/sip_profiles/external/` as a third
mount. `deploy/dev/freeswitch/` is a worked example;
[freeswitch/README.md](../freeswitch/README.md) §3 explains the layout.
