<!-- SPDX-License-Identifier: Apache-2.0 -->

# `dialplan/aicc/`

The seam. `dialplan/aicc.xml` ends with `<X-PRE-PROCESS cmd="include"
data="aicc/*.xml"/>`, and every `.xml` file in this directory is pulled into
the **`aicc`** context at that point.

This is where a deployment puts what only it has: its PSTN trunk, its carrier's
number translation, a feature code one floor needs. Nothing here is shipped —
the directory exists in the image so the include has somewhere to point, and
whatever is mounted or dropped in is the deployment's alone.

Two things to know before adding a file.

**Write rules, not a context.** A file here is included *inside* an open
`<context name="aicc">`, so it contains bare `<extension>` elements and no
`<include>` or `<context>` wrapper of its own. Two files that both open
`<context name="aicc">` do not merge: the first is used and the second is
silently ignored. Its rules stay visible to `xml_locate`, which is what makes
this so expensive to find — a trunk rule sat in a second file, every outbound
call reached the dialplan, found nothing, and hung up NO_ROUTE_DESTINATION
while the rule appeared to be right there on screen.

**Order is by filename**, and `aicc.xml`'s own rules come first. Queue
extensions (`7xxx`) and internal extensions (`10xx`) are already matched before
anything here is consulted, so a rule here cannot take them over.

A trunk rule usually looks like this — note that it passes the caller ID
through rather than pinning one, because a switch that rewrites every outbound
number to a single DID beats whatever the application decided to present:

```xml
<extension name="pstn_out">
  <condition field="destination_number" expression="^(\d{7,})$">
    <action application="set" data="effective_caller_id_number=${effective_caller_id_number}"/>
    <action application="bridge" data="sofia/gateway/my_carrier/$1"/>
  </condition>
</extension>
```

Drop the file in and `reloadxml`; no restart is needed for dialplan changes.
