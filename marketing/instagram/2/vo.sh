#!/bin/bash
# Regenerate the six VO clips: piper (male en_US ryan) + -2.5 st formant-preserving pitch shift.
# Model: https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/ryan/medium/en_US-ryan-medium.onnx
set -e
cd "$(dirname "$0")"
M=${PIPER_MODEL:-$HOME/.local/share/piper/en_US-ryan-medium.onnx}
mkdir -p vo out
i=1
while IFS= read -r line; do
  printf '%s' "$line" | piper -m "$M" -f "out/vo$i.raw.wav" --length-scale 1.10 --sentence-silence 0.10 2>/dev/null
  ffmpeg -hide_banner -loglevel error -y -i "out/vo$i.raw.wav" \
    -af "rubberband=pitch=0.865:pitchq=quality" "vo/vo$i.wav"
  echo "vo$i $(ffprobe -v error -show_entries format=duration -of csv=p=0 "vo/vo$i.wav")s"
  i=$((i+1))
done <<'EOF'
Still deploying with SSH scripts?
Build. Deploy. Verify.
Ship Go and Rust apps with zero-downtime deployments.
Monitor your services. Roll back with confidence.
Your servers. Your stack. One operational layer.
Meet Phelix.
EOF
rm -rf out
