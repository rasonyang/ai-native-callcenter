# Rolling out agent phones with Chrome policy

For many agents, use Chrome policy instead of the onboarding card. It installs
the extension, allows this host and grants the microphone, so the card has
nothing to ask. Policy is read from the registry on Windows, a managed
preferences file on macOS, and `/etc/opt/chrome/policies/managed/` on Linux.
The payload is the same:

```json
{
  "ExtensionInstallForcelist": [
    "<extension-id>;https://clients2.google.com/service/update2/crx"
  ],
  "ExtensionSettings": {
    "<extension-id>": {
      "installation_mode": "force_installed",
      "runtime_allowed_hosts": ["https://aicc.example.com"]
    }
  },
  "AudioCaptureAllowedUrls": ["https://aicc.example.com"]
}
```

| Policy | Effect |
|---|---|
| `ExtensionInstallForcelist` | Installs the extension and keeps it installed |
| `runtime_allowed_hosts` | Pre-approves the platform's origin; the extension's Allow Sites list needs no visit |
| `AudioCaptureAllowedUrls` | Grants the microphone without a prompt |

- `<extension-id>` is the Chrome Web Store ID. An unpacked development build has
  a per-machine ID that policy cannot address.
- The origin is the URL agents open. Behind a TLS proxy, that is the proxy's
  name, not this host's address.
- Check the policy names against your fleet's Chrome version. Chrome renames and
  retires policies, and silently ignores ones it does not recognise.
