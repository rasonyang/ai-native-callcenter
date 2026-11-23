# Design 08 — Real-time transcription (as built)

The 3,151-line gap analysis this file used to hold described a tree that no
longer exists: it was written before the feature shipped, and its present-tense
sections (no transcript screen, no human-leg ASR, `BOT_TRANSCRIPT`, a
`AICC_TRANSCRIBE_PARTIALS` knob) were wrong within a release. It was replaced by
this note on 2026-10-30 — the design record for a shipped feature belongs in the
code, the contract and the tests, and a copy that can drift belongs nowhere.

## What shipped, and where it lives

| Piece | Where |
|---|---|
| The tap: the switch streams the agent's leg over its WebSocket | `freeswitch/` (`mod_audio_stream` in the image), `internal/streamin` |
| Recognition: the OpenAI-Realtime transcription client and the DashScope one | `internal/transcribe` |
| Ordering: one actor per call, `seq` allocated in receipt order, finals stored, partials never stored | `internal/transcript`, table `transcripts` (migration `00009`) |
| Post-call ASR of a recording, two channels, retry and a daily cap | `internal/postcall`, tables `call_transcriptions` / `call_transcription_segments` (migration `00035`) |
| The API | `GET /calls/{callId}/transcript?sinceSeq=&limit=` (`getCallTranscript`) |
| The stream | `CALL_TRANSCRIPT` (finals and partials) and `CALL_TRANSCRIPTION_STATE` (catalog in 04 §4) |
| The screen | `web/src/components/live-transcript.tsx`, in the cockpit and on the CDR page |

Configuration is the `AICC_TRANSCRIPTION_*`, `AICC_STREAM_*`, `AICC_TRANSCRIBE_*`
and `AICC_POSTCALL_TRANSCRIBE_*` settings; `.env.example` is their registry and
their comments say what each one does. There is no partials switch: partials go
to the stream and never to the database, always.

## When a line happened

A caller line's `occurredAt` — and so its `offsetMs` — is the line's arrival
time: when the transcription reached the actor. Transcription trails speech,
and a completed transcript can land after the bot's reaction to the words, so
an arrival-stamped caller line can sit after a tool call that was a reaction
to it.

When the provider reports the speech start (`speech_started`'s `item_id` ↔
the completed transcription's `item_id`; doubao has no speech-detect event,
so its `input_audio_transcription.started` is the start), the caller's line
carries `speechStartMs` beside `offsetMs` — the speaking time as the same
kind of offset from the answer.

`seq` is not reordered for this: it is the cursor a backfill resumes from,
and a moved row is a missed line, so `seq` stays in receipt order. A caller
line whose speaking time precedes the newest `occurredAt` already stored
carries `content.transcribedLate` (and the same key on the stream payload),
so a reviewer reads the later line as the reaction it was, not an answer to
nothing.

## The module guard (the old §B.5 — `freeswitch/docker-entrypoint.sh` refers here)

The guarantee is deliberately **not** "the module is in the image" — that is a
property of whoever built the image, and a future image can drop it silently.
The guarantee is **a stack configured to transcribe cannot start without it**,
which survives changes to the image because it is checked at every boot.

| Configuration | Result |
|---|---|
| enabled, module absent | **refuses to start**, exit 1, naming what would otherwise happen |
| disabled, module absent | starts, exit 0, logs "live transcription disabled" |
| enabled, module present | starts, exit 0, and `<load module="mod_audio_stream"/>` is in `modules.conf.xml` |

Without it the failure is the quiet one: the application healthy, the switch
healthy, calls connecting, agents answering, and the transcript panel saying
"Connecting…" for ever while nothing anywhere reports a fault — because from
each component's own point of view there is not one.

`freeswitch/assert-audio-stream.sh` checks the other half at build time: the
module must **declare** libspeexdsp. Undefined `speex_resampler_*` symbols are
normal for a dynamically linked module and resolve at load, so a build that never
linked the library looks fine (`module_exists` true, attach returning `+OK`) and
then forwards frames at the wrong rate under the right label.
