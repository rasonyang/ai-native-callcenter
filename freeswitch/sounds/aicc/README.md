# Bot-unavailable prompts

The prompt the switch plays when the bot leg fails and the dialled number has
no fallback queue. Inbound calls get it from `freeswitch/scripts/aicc_inbound.lua`;
outbound calls get it from the application (#459).

The language is picked from a fixed map: `zh` plays `zh/`, anything else plays
`en/`.

## Path contract

`<sounds_dir>/aicc/<lang>/bot-unavailable.wav`, with `<lang>` one of `zh` or `en`.
Each file is 8 kHz, mono, 16-bit PCM WAV (RIFF, format tag 1), 1.5 to 8 s, peak
about -3 dBFS, with roughly 100 ms of padding at each end. `sounds_test.go`
checks the format, the length and that neither end has more than 200 ms of
silence. Only the `.wav` files live in `zh/` and `en/`: the Docker build
excludes `sounds/aicc/*.*` and copies the directories.

## Prompts

| File | Text |
|---|---|
| `zh/bot-unavailable.wav` | 抱歉，系统暂时无法为您服务，请稍后再试。再见。 |
| `en/bot-unavailable.wav` | Sorry, we are unable to continue this call right now. Please try again later. Goodbye. |

## How they were made

- Engine: Qwen3-TTS, run locally with MLX through mlx-audio 0.6.1.
- Model: `mlx-community/Qwen3-TTS-12Hz-1.7B-CustomVoice-8bit`, snapshot
  `41d3337e8b7f2843a75841595fc14e4b9a7a4b96`. Licence Apache-2.0, see the
  [model card](https://huggingface.co/mlx-community/Qwen3-TTS-12Hz-1.7B-CustomVoice-8bit).
- Speaker `vivian` for both languages; `lang_code` `chinese` for the zh text and
  `english` for the en text. Other sampling parameters are the model defaults.
- Seed: 3 (`mx.random.seed(3)` before each prompt). The takes of seeds 1 to 3
  all transcribed back exactly; seed 3 was kept because the en take fits the
  8 s limit with room to spare.
- Verification: both files were transcribed back with Qwen3-ASR-1.7B-8bit and
  whisper-large-v3-turbo, and matched the text exactly (ignoring punctuation
  width).

Generation is sampled, so another machine or mlx-audio version may produce a
different take from the same seed. The committed WAV files are the source of
truth; regenerating is not required.

## Regenerate

Run in this directory, with sox installed:

```sh
OUT=$(mktemp -d)
uv run --python 3.12 --with mlx-audio==0.6.1 --with soundfile \
  python generate.py "$OUT" 3
for l in zh en; do
  sox "$OUT/$l-raw.wav" -b 16 -e signed-integer "$OUT/$l-mid.wav" \
    silence 1 0.05 0.5% reverse silence 1 0.05 0.5% reverse rate -v 8000 channels 1
  sox "$OUT/$l-mid.wav" "$l/bot-unavailable.wav" norm -3 pad 0.1 0.1
done
```

The first `sox` step trims leading and trailing silence and resamples from
24 kHz at very high quality; the second normalises to -3 dBFS and adds 100 ms
at each end.

## Licence

The recordings are distributed under this repository's Apache-2.0 licence.
