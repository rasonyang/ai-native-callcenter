-- SPDX-License-Identifier: Apache-2.0
--
-- mod_callcenter keeps its own agents/tiers/members tables, unqualified, in
-- whatever database it is pointed at. Those names collide with the
-- application's, so the switch gets a database of its own (design 03, M0
-- finding). It is created empty; mod_callcenter builds its schema on load.
CREATE DATABASE aicc_fs OWNER aicc;
