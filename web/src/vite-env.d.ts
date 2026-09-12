/// <reference types="vite/client" />

/**
 * Build-time settings. The only one this application has is the published id
 * of the phone extension, which differs between a Web Store release and an
 * unpacked build a developer loaded themselves.
 */
interface ImportMetaEnv {
  readonly VITE_WEB_SIP_PHONE_ID?: string
}

interface ImportMeta {
  readonly env: ImportMetaEnv
}
