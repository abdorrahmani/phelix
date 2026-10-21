#!/usr/bin/env python3
"""Phelix reel 28s: original music bed + piper VO mix. numpy + wave only."""
import numpy as np, wave, sys, os

SR = 44100
DUR = 28.0
N = int(SR * DUR)
rng = np.random.default_rng(11)

def midi(n):
    return 440.0 * 2 ** ((n - 69) / 12)

def place(bus, sig, t0, gain=1.0):
    i = int(t0 * SR)
    if i >= len(bus):
        return
    j = min(len(bus), i + len(sig))
    bus[i:j] += sig[: j - i] * gain

def adsr(n, a, d, s, r, sus=0.7):
    a, d, r = int(a * SR), int(d * SR), int(r * SR)
    sus_n = max(0, n - a - d - r)
    e = np.concatenate([
        np.linspace(0, 1, max(1, a), endpoint=False),
        np.linspace(1, sus, max(1, d), endpoint=False),
        np.full(sus_n, sus),
        np.linspace(sus, 0, max(1, r)),
    ])
    return e[:n] if len(e) >= n else np.pad(e, (0, n - len(e)))

# ---- instruments ----
def pad(freq, n, amp=0.1):
    t = np.arange(n) / SR
    sig = (np.sin(2 * np.pi * freq * t) * 0.6 +
           np.sin(2 * np.pi * freq * 1.004 * t) * 0.25 +
           np.sin(2 * np.pi * freq * 0.996 * t) * 0.25 +
           0.10 * np.sin(2 * np.pi * freq * 2 * t) +
           0.05 * np.sin(2 * np.pi * freq * 3 * t))
    return sig * adsr(n, 0.9, 0.6, 0.82, 1.4, 0.85) * amp

def pluck(freq, n, amp=0.1):
    t = np.arange(n) / SR
    sig = np.sin(2 * np.pi * freq * t) + 0.35 * np.sin(2 * np.pi * freq * 2 * t) + 0.12 * np.sin(2 * np.pi * freq * 3 * t)
    return sig * np.exp(-t * 7.0) * amp

def kick(n, amp=0.22):
    t = np.arange(n) / SR
    f = 120 * np.exp(-t * 32) + 42
    return np.sin(2 * np.pi * np.cumsum(f) / SR) * np.exp(-t * 9) * amp

def hat(n, amp=0.03):
    t = np.arange(n) / SR
    noise = np.diff(np.concatenate([[0], rng.standard_normal(n)]))
    return noise * np.exp(-t * 60) * amp

def impact(n, amp=0.6):
    t = np.arange(n) / SR
    f = 70 * np.exp(-t * 6) + 30
    sub = np.sin(2 * np.pi * np.cumsum(f) / SR) * np.exp(-t * 2.4)
    click = np.diff(np.concatenate([[0], rng.standard_normal(n)])) * np.exp(-t * 30) * 0.35
    return (sub + click) * amp

def riser(n, amp=0.18):
    t = np.arange(n) / SR
    noise = rng.standard_normal(n)
    # sweep a one-pole lowpass upward
    y = np.zeros(n)
    a = 0.004 + 0.5 * (t / t[-1]) ** 2
    prev = 0.0
    for i in range(0, n, 1):
        prev += a[i] * (noise[i] - prev)
        y[i] = prev
    return y * np.linspace(0.05, 1, n) ** 2 * amp

def tick(n, amp=0.05, f=2400):
    t = np.arange(n) / SR
    return (np.sin(2 * np.pi * f * t) * 0.6 + rng.standard_normal(n) * 0.4) * np.exp(-t * 90) * amp

def beep(f, n, amp=0.05):
    t = np.arange(n) / SR
    return np.sin(2 * np.pi * f * t) * np.minimum(1, t * 200) * np.exp(-t * 6) * amp

def shimmer(n, amp=0.12):
    t = np.arange(n) / SR
    s = np.zeros(n)
    for k, f in enumerate([midi(76), midi(83), midi(88), midi(93)]):
        s += np.sin(2 * np.pi * f * t) * np.exp(-t * (1.1 + k * 0.25)) * (0.6 ** k)
    return s * amp

# ---- score ----
BPM = 120.0
beat = 60.0 / BPM  # 0.5 s
music = np.zeros(N + SR * 2)
t_all = np.arange(N) / SR

# harmony: tension (Am) -> lift (F, C, G) -> resolve (Am add9)
CH = {
    'Am': ([57, 60, 64], 45),
    'F':  ([53, 57, 60], 41),
    'C':  ([48, 52, 55], 36),
    'G':  ([55, 59, 62], 43),
}
prog = [('Am', 0.0, 4.0), ('F', 4.0, 6.0), ('C', 6.0, 8.0),
        ('Am', 8.0, 10.0), ('F', 10.0, 12.0), ('C', 12.0, 14.0), ('G', 14.0, 16.0),
        ('Am', 16.0, 18.0), ('F', 18.0, 20.0),
        ('C', 20.0, 23.0), ('Am', 23.0, 28.0)]
for name, a, b in prog:
    notes, bass = CH[name]
    n = int((b - a + 1.0) * SR)
    g = 0.10 if a < 4 else (0.075 if a < 20 else 0.095)
    for nt in notes:
        place(music, pad(midi(nt), n, g), a)
    place(music, pad(midi(bass), n, g * 1.15), a)

# 0-4: tension. sub pulse, ticks, alarms
for k in range(8):
    t0 = k * 0.5
    n = int(0.42 * SR)
    f = midi(33)
    tt = np.arange(n) / SR
    place(music, np.sin(2 * np.pi * f * tt) * np.exp(-tt * 7) * 0.14, t0)
for k in range(int(4.0 / 0.25)):
    place(music, tick(int(0.05 * SR), 0.030 + 0.02 * rng.random()), k * 0.25 + rng.random() * 0.03)
place(music, beep(880, int(0.16 * SR), 0.045), 1.05)
place(music, beep(660, int(0.16 * SR), 0.045), 1.28)
place(music, beep(520, int(0.22 * SR), 0.035), 3.2)
# 3.0-4.0 riser into the drop
place(music, riser(int(1.0 * SR), 0.16), 3.0)
# 4.0 impact
place(music, impact(int(2.2 * SR), 0.55), 4.0)
place(music, shimmer(int(2.0 * SR), 0.055), 4.05)

# 5.0-8.0: pulse arpeggio building
for k in range(int(3.0 / (beat / 2))):
    t0 = 5.0 + k * beat / 2
    notes = CH['F'][0] if t0 < 6 else CH['C'][0]
    nt = notes[k % 3] + 12
    place(music, pluck(midi(nt), int(beat * 0.9 * SR), 0.035 + 0.01 * (k / 12)), t0)

# 8.0: transition hit
place(music, impact(int(1.4 * SR), 0.28), 8.0)

# 8.0-20.0: groove
for k in range(int((20.0 - 8.0) / beat)):
    t0 = 8.0 + k * beat
    place(music, kick(int(0.30 * SR), 0.17), t0)
    place(music, hat(int(0.10 * SR), 0.026), t0 + beat / 2)
# bass pulse on beats 1 and 3
for k in range(int((20.0 - 8.0) / (2 * beat))):
    t0 = 8.0 + k * 2 * beat
    bar = int((t0 - 8.0) // 2)
    bass = prog[min(len(prog) - 1, bar + 3)][1] if bar + 3 < len(prog) else 45
    n = int(beat * 1.5 * SR)
    tt = np.arange(n) / SR
    f = midi(bass)
    bs = np.sin(2 * np.pi * f * tt) * np.exp(-tt * 2.4) * 0.14
    bs += 0.4 * np.sin(2 * np.pi * f * 2 * tt) * np.exp(-tt * 3.5) * 0.07
    place(music, bs, t0)
# 8th-note arp
for k in range(int((20.0 - 8.0) / (beat / 2))):
    t0 = 8.0 + k * beat / 2
    seg = [p for p in prog if p[1] <= t0 < p[2]]
    notes = CH[seg[0][0]][0] if seg else CH['Am'][0]
    nt = notes[k % 3] + 12
    place(music, pluck(midi(nt), int(beat * 0.85 * SR), 0.030), t0)

# accents tied to the on-screen beats
place(music, pluck(midi(69), int(0.5 * SR), 0.05), 9.0)   # build lands
place(music, pluck(midi(72), int(0.5 * SR), 0.05), 10.0)  # compiled
place(music, tick(int(0.10 * SR), 0.06, 3200), 11.2)      # health check
place(music, beep(1320, int(0.18 * SR), 0.05), 11.9)      # healthy
place(music, impact(int(0.8 * SR), 0.16), 12.5)           # traffic switch
place(music, tick(int(0.10 * SR), 0.06, 2800), 17.6)      # rollback confirm

# 20.0 final hit + shimmer
place(music, impact(int(3.0 * SR), 0.50), 20.0)
place(music, shimmer(int(4.5 * SR), 0.10), 20.08)
place(music, riser(int(0.7 * SR), 0.10), 19.3)

music = music[:N]

# ---- reverb (FFT convolution, one shared IR) ----
ir_n = int(1.6 * SR)
t_ir = np.arange(ir_n) / SR
ir = rng.standard_normal(ir_n) * np.exp(-t_ir * 4.2)
ir[:200] *= np.linspace(0, 1, 200)
ir /= np.sqrt(np.sum(ir ** 2))
L = 1 << int(np.ceil(np.log2(N + ir_n)))
wet = np.fft.irfft(np.fft.rfft(music, L) * np.fft.rfft(ir, L))[:N]
wet /= np.max(np.abs(wet)) + 1e-9
music = music + 0.16 * wet * np.max(np.abs(music))

def load_wav(path):
    """Read a mono float signal at SR, resampling from the file's own rate."""
    with wave.open(path, 'rb') as f:
        sr = f.getframerate()
        d = np.frombuffer(f.readframes(f.getnframes()), dtype='<i2').astype(np.float64) / 32768
        if f.getnchannels() == 2:
            d = d.reshape(-1, 2).mean(axis=1)
    if sr != SR:  # piper writes 22050; play it back at its own rate or it runs 2x fast
        n = int(round(len(d) * SR / sr))
        d = np.interp(np.linspace(0, len(d) - 1, n), np.arange(len(d)), d)
    return d

# ---- VO ----
VO = [(1, 0.55), (2, 7.95), (3, 10.35), (4, 14.20), (5, 20.25), (6, 24.95)]
vo_bus = np.zeros(N + SR)
here = os.path.dirname(os.path.abspath(__file__))
for k, t0 in VO:
    d = load_wav(os.path.join(here, 'vo', f'vo{k}.wav'))
    rms = np.sqrt(np.mean(d ** 2)) + 1e-9
    d = d * (0.085 / rms)
    place(vo_bus, d, t0)

# duck the music under the voice: envelope follower with 180 ms attack/release
env = np.abs(vo_bus)
k_ = int(0.18 * SR)
env = np.convolve(env, np.ones(k_) / k_, mode='same')
env = env / (env.max() + 1e-9)
duck = 1 - 0.55 * np.clip(env * 5.0, 0, 1)
music *= duck[:N]

mix = music + vo_bus[:N]

# ---- master ----
fade = np.ones(N)
fi, fo = int(0.02 * SR), int(1.6 * SR)
fade[:fi] = np.linspace(0, 1, fi)
fade[-fo:] = np.linspace(1, 0, fo) ** 1.4
mix *= fade
mix = np.tanh(mix * 1.25) * 0.9

r = np.roll(mix, int(0.011 * SR)) * 0.97
stereo = np.stack([mix, r], axis=1)
pcm = (np.clip(stereo, -1, 1) * 32767).astype('<i2')
out = sys.argv[1] if len(sys.argv) > 1 else 'mix_raw.wav'
with wave.open(out, 'wb') as f:
    f.setnchannels(2); f.setsampwidth(2); f.setframerate(SR)
    f.writeframes(pcm.tobytes())
print('wrote', out, round(len(pcm) / SR, 2), 's')
