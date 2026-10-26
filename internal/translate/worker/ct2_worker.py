import json
import os
import sys

import ctranslate2
import sentencepiece as spm

_loaded = {}


def load(dirpath):
    entry = _loaded.get(dirpath)
    if entry is None:
        sp_src = spm.SentencePieceProcessor()
        sp_src.Load(os.path.join(dirpath, "source.spm"))
        sp_tgt = spm.SentencePieceProcessor()
        sp_tgt.Load(os.path.join(dirpath, "target.spm"))
        tr = ctranslate2.Translator(dirpath, device="cpu")
        entry = (sp_src, sp_tgt, tr)
        _loaded[dirpath] = entry
    return entry


def translate(dirpath, texts):
    sp_src, sp_tgt, tr = load(dirpath)
    batch = []
    passthrough = []
    for text in texts:
        toks = sp_src.encode(text, out_type=str)
        if toks:
            batch.append(toks + ["</s>"])
            passthrough.append(False)
        else:
            batch.append(["</s>"])
            passthrough.append(True)
    results = tr.translate_batch(batch, beam_size=4)
    out = []
    for i, res in enumerate(results):
        if passthrough[i]:
            out.append(texts[i])
            continue
        hyp = [t for t in res.hypotheses[0] if t != "</s>"]
        out.append(sp_tgt.decode(hyp))
    return out


def main():
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except ValueError:
            continue
        resp = {"id": req.get("id", 0)}
        try:
            resp["segments"] = translate(req["dir"], req["segments"])
        except Exception as exc:
            resp["error"] = str(exc)
        sys.stdout.write(json.dumps(resp, ensure_ascii=False) + "\n")
        sys.stdout.flush()


if __name__ == "__main__":
    main()
