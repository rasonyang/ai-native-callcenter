<!-- SPDX-License-Identifier: Apache-2.0 -->

# The simulated public network

Two directories, both mounted read-only onto the switch by
`deploy/docker-compose.yml`:

* `directory/customers.xml` holds eighteen customer telephones any SIP softphone
  can register as, password `aicc@123`, each carrying its own number as caller
  id and arriving in the `public` context the way a carrier's call does.
* `dialplan/10_simulated_pstn.xml` routes both ways: what a customer dials goes to the inbound
  doorway; what the platform dials to a customer number rings that registered
  telephone, and fails as `USER_NOT_REGISTERED` when nobody holds it.

Together they let the whole product be exercised on one machine with no
carrier. A real deployment replaces these files with its trunk in the same two
directories, and mounts its gateway into `conf/sip_profiles/external/` as a
third. `deploy/dev/freeswitch/` is a worked example and
[`freeswitch/README.md`](../../freeswitch/README.md) §3 explains the seam.
