# SPDX-License-Identifier: Apache-2.0
"""Regenerate the bot-unavailable prompts (see README.md).

    uv run --python 3.12 --with mlx-audio==0.6.1 --with soundfile python generate.py OUT_DIR [SEED]

Writes OUT_DIR/{zh,en}-raw.wav (24 kHz float, straight from the model). The sox
post-processing in README.md turns them into the committed 8 kHz files.
"""
import sys

import mlx.core as mx
import numpy as np
import soundfile as sf
from mlx_audio.tts.utils import load_model

MODEL = "mlx-community/Qwen3-TTS-12Hz-1.7B-CustomVoice-8bit"
SPEAKER = "vivian"
PROMPTS = {
    "zh": ("chinese", "抱歉，系统暂时无法为您服务，请稍后再试。再见。"),
    "en": ("english", "Sorry, we are unable to continue this call right now. Please try again later. Goodbye."),
}

out = sys.argv[1]
seed = int(sys.argv[2]) if len(sys.argv) > 2 else 485
model = load_model(MODEL)
for lang, (lang_code, text) in PROMPTS.items():
    mx.random.seed(seed)
    chunks = [np.array(r.audio) for r in model.generate(text=text, voice=SPEAKER, lang_code=lang_code)]
    sf.write(f"{out}/{lang}-raw.wav", np.concatenate(chunks), model.sample_rate)
    print(lang, seed, model.sample_rate)
