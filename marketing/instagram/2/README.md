# Phelix — 28s Instagram Reel

**File:** `phelix-reel-28s.mp4` — 1080x1920 (9:16), 30 fps, 28.0 s, H.264 High@4.2 + AAC 48 kHz stereo, -14.8 LUFS, ~7 MB.
**Cover:** `phelix-cover.png` (frame at 0:12, deployment switch — use as the Reels cover).
**Source:** `build-reel.js` (visuals) + `audio.py` (music bed + VO mix) + `logo.png`.

## Voiceover

Local piper TTS, male voice `en_US-ryan-medium`, no cloud service, then a -2.5 semitone
formant-preserving pitch shift (`rubberband`) so the read sits around 130-160 Hz instead of
165-180 Hz. Six lines, mixed at the scene beats and sidechain-ducked under the music in
`audio.py`. `vo.sh` regenerates every clip from the six lines below; the model is expected at
`~/.local/share/piper/en_US-ryan-medium.onnx` (override with `PIPER_MODEL`).

**Sample-rate note:** piper writes 22050 Hz mono. `audio.py:load_wav` resamples to the 44100 Hz
mix bus. Skipping that step plays the voice 2x fast and one octave up.

| Line | Start | Source length |
|---|---|---|
| Still deploying with SSH scripts? | 0.55 | 2.15 s |
| Build. Deploy. Verify. | 7.95 | 1.52 s |
| Ship Go and Rust apps with zero-downtime deployments. | 10.35 | 3.18 s |
| Monitor your services. Roll back with confidence. | 14.20 | 2.63 s |
| Your servers. Your stack. One operational layer. | 20.25 | 2.88 s |
| Meet Phelix. | 24.95 | 0.85 s |

`vo.sh` uses `--length-scale 1.10`; raise it for a slower read (never above ~1.25 or lines
overrun their scenes), then re-run `audio.py` and the encode.

## Regenerate

```bash
node build-reel.js                       # 840 SVGs into ./frames
mkdir -p png
find frames -name '*.svg' -print0 | xargs -0 -P 12 -I{} bash -c \
  'f="$1"; rsvg-convert -w 1080 -h 1920 "$f" -o "png/$(basename "$f" .svg).png"' _ {}
./vo.sh                                  # six VO clips (piper + pitch shift); skip if vo/ is current
python3 -I audio.py mix_raw.wav
ffmpeg -y -i mix_raw.wav -af loudnorm=I=-14:TP=-1.5:LRA=11 -ar 48000 final_audio.wav
ffmpeg -y -framerate 30 -i png/f_%04d.png -i final_audio.wav \
  -c:v libx264 -preset slow -crf 18 -pix_fmt yuv420p -profile:v high -level 4.2 \
  -c:a aac -b:a 192k -ar 48000 -movflags +faststart -r 30 -t 28 phelix-reel-28s.mp4
```

`build-reel.js <seconds>` writes a single `preview_<t>.svg` instead of the whole sequence.

## Shot list

| Time | Beat | On screen |
|---|---|---|
| 0.0–4.0 | CHAOS | Full-bleed terminal, five hard cuts (ssh, release.sh, systemctl restart, journalctl errors, deploy failed). Red accents. "Still deploying with SSH scripts?" |
| 4.0–8.0 | TRANSFORM | Error rows freeze, collapse, converge into a lime-to-cyan beam. Logo reveal. "Your deployment workflow needs an upgrade." |
| 8.0–14.0 | CONTROL | `phelix build api --port 8080` lands v5 as a non-current build. v4 live / v5 candidate side by side, health check spins to a check, proxy traffic 0→100%. "Build. Deploy. Verify." then "Zero-Downtime Deployments." |
| 14.0–20.0 | OBSERVE | Dark monitoring dashboard: status, uptime, req/s, CPU/mem sparklines, deployment history. Then version timeline and a controlled `rollback --to v4` with traffic, binary and env all reverting. "Monitor Everything." then "Roll Back with Confidence." |
| 20.0–28.0 | BRAND | Interface collapses, light wave traces the frame, logo reveal with masked sheen, slow push-in. "YOUR SERVERS. YOUR STACK." / "ONE OPERATIONAL LAYER." / `phelix.anophel.com`. |

## Caption

Your servers. Your stack. One operational layer.

Phelix is one binary that builds, versions and deploys your Go and Rust apps on your own host,
and only cuts traffic to a new version after it passes its health check.

Rollback is one command. Monitoring is built in.

phelix.anophel.com

#golang #rustlang #devops #selfhosted #zerodowntime #cli #deployment

## Notes

- Logo is the supplied PNG, unmodified, at every appearance (scene 2, scene 5, cover).
- All copy sits between y=270 and y=1480, clear of Reels and Stories overlays.
- No claims beyond the product: no uptime numbers, no customer logos, no benchmark figures.
- The dashboard figures (uptime `2d 04h`, `246 MB`, `REQ / S`) are illustrative UI, not claims.
- Audio is baked in. Instagram's picker will replace it if the user adds a trending track.
