// Phelix Instagram reel. 28s, 1080x1920, 30fps. CHAOS -> CONTROL -> CONFIDENCE.
const fs = require('fs');
const DIR = __dirname;
const W = 1080, H = 1920, FPS = 30, DUR = 28;

const INK = '#050709', NAVY = '#0B1220';
const LIME = '#A3E635', EMERALD = '#34D399', CYAN = '#22D3EE';
const WHITE = '#F2F7F7', MUTED = '#7C8C95', RED = '#FF5C5C';
const FONT = 'Space Grotesk', MONO = 'JetBrains Mono';

const clamp = (x, a = 0, b = 1) => Math.min(b, Math.max(a, x));
const smooth = (x) => { x = clamp(x); return x * x * (3 - 2 * x); };
const easeOut = (x) => 1 - Math.pow(1 - clamp(x), 3);
const easeIn = (x) => Math.pow(clamp(x), 3);
const lerp = (a, b, t) => a + (b - a) * t;
const S = (t, a, b) => smooth((t - a) / (b - a));
const win = (t, a, b, fi = 0.3, fo = 0.3) => S(t, a, a + fi) * (1 - S(t, b - fo, b));
const esc = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const txt = (s, x, y, o = {}) => `<text x="${x}" y="${y}" font-family="${o.family || FONT}" font-size="${o.size || 48}" font-weight="${o.weight || 500}" fill="${o.fill || WHITE}" text-anchor="${o.anchor || 'middle'}" opacity="${o.op != null ? o.op : 1}"${o.ls != null ? ` letter-spacing="${o.ls}"` : ''}${o.rotate ? ` transform="rotate(${o.rotate} ${x} ${y})"` : ''}>${esc(s)}</text>`;

const LOGO = fs.readFileSync(DIR + '/logo.png').toString('base64');
const LAR = 892 / 1156;

const DEFS = `<defs>
  <linearGradient id="brand" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="${LIME}"/><stop offset=".55" stop-color="${EMERALD}"/><stop offset="1" stop-color="${CYAN}"/></linearGradient>
  <linearGradient id="brandH" x1="0" y1="0" x2="1" y2="0"><stop offset="0" stop-color="${LIME}"/><stop offset="1" stop-color="${CYAN}"/></linearGradient>
  <linearGradient id="fadeW" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#fff" stop-opacity="0"/><stop offset=".25" stop-color="#fff"/><stop offset=".75" stop-color="#fff"/><stop offset="1" stop-color="#fff" stop-opacity="0"/></linearGradient>
  <linearGradient id="glass" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#16222C"/><stop offset="1" stop-color="#0A1118"/></linearGradient>
  <radialGradient id="orbL"><stop offset="0" stop-color="${LIME}" stop-opacity=".22"/><stop offset="1" stop-color="${LIME}" stop-opacity="0"/></radialGradient>
  <radialGradient id="orbC"><stop offset="0" stop-color="${CYAN}" stop-opacity=".20"/><stop offset="1" stop-color="${CYAN}" stop-opacity="0"/></radialGradient>
  <radialGradient id="orbR"><stop offset="0" stop-color="${RED}" stop-opacity=".16"/><stop offset="1" stop-color="${RED}" stop-opacity="0"/></radialGradient>
  <radialGradient id="vig" cx="50%" cy="46%" r="75%"><stop offset="45%" stop-color="#000" stop-opacity="0"/><stop offset="100%" stop-color="#000" stop-opacity=".72"/></radialGradient>
  <radialGradient id="logoGlow"><stop offset="0" stop-color="${LIME}" stop-opacity=".55"/><stop offset=".45" stop-color="${EMERALD}" stop-opacity=".22"/><stop offset="1" stop-color="${CYAN}" stop-opacity="0"/></radialGradient>
  <filter id="blur8" x="-60%" y="-60%" width="220%" height="220%"><feGaussianBlur stdDeviation="8"/></filter>
  <filter id="blur20" x="-60%" y="-60%" width="220%" height="220%"><feGaussianBlur stdDeviation="20"/></filter>
  <filter id="blur40" x="-80%" y="-80%" width="260%" height="260%"><feGaussianBlur stdDeviation="42"/></filter>
  <filter id="shadow" x="-30%" y="-30%" width="160%" height="160%"><feDropShadow dx="0" dy="24" stdDeviation="26" flood-color="#000" flood-opacity=".55"/></filter>
  <filter id="grain"><feTurbulence type="fractalNoise" baseFrequency=".85" numOctaves="3"/><feColorMatrix type="saturate" values="0"/></filter>
  <mask id="logoMask"><image href="data:image/png;base64,${LOGO}" width="892" height="1156"/></mask>
  <pattern id="grid" width="72" height="72" patternUnits="userSpaceOnUse"><path d="M72 0H0V72" fill="none" stroke="#9FE7DA" stroke-opacity=".045"/></pattern>
  <pattern id="gridFine" width="24" height="24" patternUnits="userSpaceOnUse"><path d="M24 0H0V24" fill="none" stroke="#9FE7DA" stroke-opacity=".03"/></pattern>
</defs>`;

function bg(T, opts = {}) {
  const red = opts.red || 0, bright = opts.bright || 0;
  const ox = Math.sin(T * 0.28) * 120, oy = Math.cos(T * 0.21) * 100;
  return `<rect width="${W}" height="${H}" fill="${INK}"/>
  <rect width="${W}" height="${H}" fill="${NAVY}" opacity="${0.55 + 0.25 * bright}"/>
  <rect y="${H * 0.18}" width="${W}" height="${H * 0.7}" fill="url(#grid)" opacity="${0.5 + 0.5 * bright}"/>
  <rect y="${H * 0.18}" width="${W}" height="${H * 0.7}" fill="url(#gridFine)" opacity="0.5"/>
  <circle cx="${300 + ox}" cy="${560 + oy}" r="620" fill="url(#orbL)" opacity="${0.7 + 0.3 * bright}"/>
  <circle cx="${830 - ox}" cy="${1320 - oy}" r="660" fill="url(#orbC)" opacity="${0.7 + 0.3 * bright}"/>
  ${red ? `<circle cx="540" cy="900" r="700" fill="url(#orbR)" opacity="${red}"/>` : ''}
  ${bright < 0.5 ? `<rect width="${W}" height="${H}" fill="${INK}" opacity="${0.55 - bright}"/>` : ''}`;
}

function logoImg(cx, cy, h, op = 1, glow = 1, rot = 0, sc = 1) {
  const w = h * LAR;
  return `<g opacity="${op}" transform="translate(${cx},${cy}) rotate(${rot}) scale(${sc})">
    <ellipse cx="0" cy="${h * 0.08}" rx="${w * 0.85}" ry="${h * 0.62}" fill="url(#logoGlow)" opacity="${0.9 * glow}"/>
    <image href="data:image/png;base64,${LOGO}" x="${-w / 2}" y="${-h / 2}" width="${w}" height="${h}"/>
  </g>`;
}

// ---------- shared UI bits ----------
function card(x, y, w, h, col = '#22323C', op = 1, r = 26, fill = '#0B141A') {
  return `<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="${r}" fill="${fill}" stroke="${col}" stroke-opacity="${0.55 * op}"/>`;
}
function dot(x, y, r, fill, op = 1) { return `<circle cx="${x}" cy="${y}" r="${r}" fill="${fill}" opacity="${op}"/>`; }
function check(x, y, s, col, p = 1) {
  const L = 54 * s;
  return `<path d="M${x - L * 0.4} ${y} l${L * 0.28} ${L * 0.3} l${L * 0.55} -${L * 0.62}" fill="none" stroke="${col}" stroke-width="${7 * s}" stroke-linecap="round" stroke-linejoin="round" stroke-dasharray="100" stroke-dashoffset="${100 * (1 - p)}"/>`;
}

// ---------- Scene 1: 0-4s THE PAIN ----------
function term1(t, T) {
  // full-bleed terminal, 5 rapid cuts
  const cuts = [0, 0.95, 1.8, 2.55, 3.15, 4.0];
  let i = 0;
  while (i < cuts.length - 2 && T >= cuts[i + 1]) i++;
  const ct = T - cuts[i];
  const jx = Math.sin(T * 41) * 2.6, jy = Math.cos(T * 33) * 2.2;
  const flash = Math.max(0, 1 - ct / 0.12);

  const base = `<rect width="${W}" height="${H}" fill="#070A0C"/>
    <rect x="40" y="200" width="1000" height="780" rx="22" fill="#080D10" stroke="#1B2429"/>`;
  const head = (label, right) => `<rect x="40" y="200" width="1000" height="64" rx="22" fill="#0E1518"/><rect x="40" y="242" width="1000" height="22" fill="#0E1518"/>
    ${dot(78, 232, 8, '#FF5F57')}${dot(104, 232, 8, '#FEBC2E')}${dot(130, 232, 8, '#28C840')}
    ${txt(label, 560, 245, { family: MONO, size: 23, fill: '#5C7078' })}
    ${txt(right, 1006, 245, { family: MONO, size: 21, fill: '#6B3A3A', anchor: 'end' })}`;

  let body = '';
  if (i === 0) {
    const lines = [
      ['$ ssh deploy@prod-01', '#DDE7E6', 0.0],
      ['deploy@prod-01:~$ ./release.sh', '#DDE7E6', 0.16],
      ['pulling build ... 4m12s', '#8FA0A8', 0.34],
      ['WARN connection reset by peer', '#FFB067', 0.55],
      ['retrying ...', '#8FA0A8', 0.7],
    ];
    body = lines.map(([s, c, at], k) => {
      const p = S(ct, at, at + 0.12);
      return txt(s, 80, 340 + k * 60, { family: MONO, size: 33, fill: c, anchor: 'start', op: p });
    }).join('') + `<rect x="80" y="${308 + 5 * 60}" width="${340}" height="40" fill="#DDE7E6" opacity="${Math.floor(T * 3) % 2 ? 0.75 : 0}"/>`;
  } else if (i === 1) {
    body = `<rect x="40" y="264" width="1000" height="716" fill="#060A0C"/>
    ${[['#!/bin/bash', '#4E6068'], ['set -e', '#4E6068'], ['', ''], ['git pull origin main', '#DDE7E6'], ['go build -o app ./cmd/api', '#DDE7E6'], ['systemctl stop api', '#FF8E8E'], ['cp app /opt/api/app', '#DDE7E6'], ['systemctl start api', '#FFB067'], ['curl localhost:8080/health', '#DDE7E6'], ['', ''], ['# hope nobody hits it mid-restart', '#5D4A4A']].map(([s, c], k) => txt(s, 78, 340 + k * 58, { family: MONO, size: 30, fill: c, anchor: 'start' })).join('')}
    <rect x="40" y="${340 + 5 * 58 - 38}" width="1000" height="56" fill="${RED}" opacity="0.07"/>
    <rect x="40" y="${340 + 7 * 58 - 38}" width="1000" height="56" fill="${RED}" opacity="0.07"/>`;
  } else if (i === 2) {
    body = `${['deploy@prod-01:~$ systemctl restart api', '', '[ ok ] stopping api ...', '[ ok ] starting api ...', '', 'deploy@prod-01:~$ uptime', ' 02:14:07 up 0 min, 1 user', 'deploy@prod-01:~$ curl -s -o /dev/null -w "%{http_code}" localhost:8080', '502'].map((s, k) => txt(s, 80, 350 + k * 68, { family: MONO, size: k === 8 ? 62 : 32, fill: k === 8 ? RED : (k === 7 ? '#E0E9E8' : '#8FA0A8'), anchor: 'start', weight: k === 8 ? 700 : 400 })).join('')}
    ${txt('502', 940, 940, { family: MONO, size: 200, weight: 700, fill: RED, op: 0.10 })}`;
  } else if (i === 3) {
    body = `${['02:14:07 ERROR api exited status=1', '02:14:07 ERROR bind: address already in use', '02:14:08 WARN proxy upstream 502', '02:14:08 ERROR api exited status=1', '02:14:09 ERROR port 8080 in use', '02:14:09 WARN retry 1/5', '02:14:12 ERROR api exited status=1', '02:14:13 ERROR rollback: no previous build', '02:14:15 WARN 43 requests failed'].map((s, k) => txt(s, 78, 340 + k * 62, { family: MONO, size: 28, fill: k % 3 === 0 ? '#FF7A7A' : (k % 3 === 1 ? '#C88A5A' : '#7E9098'), anchor: 'start', op: 0.55 + 0.45 * S(ct, k * 0.06, k * 0.06 + 0.1) })).join('')}`;
  } else {
    const p = clamp(ct / 0.55);
    body = `<rect x="40" y="264" width="1000" height="716" fill="#0A0709"/>
    ${txt('deploy failed', 540, 560, { size: 104, weight: 700, fill: '#FF6B6B', op: p })}
    ${txt('release 0.0.4 exited before health check', 540, 632, { family: MONO, size: 26, fill: '#B08A8A', op: p })}
    ${[['api', 'exited 1'], ['proxy', '502 x43'], ['rollback', 'none']].map(([a, b], k) => `<g opacity="${p}">${card(200 + k * 240, 740, 220, 118, '#3A2626', 1, 18, '#130E10')}${txt(a, 310 + k * 240, 790, { family: MONO, size: 22, fill: '#8C7575' })}${txt(b, 310 + k * 240, 828, { family: MONO, size: 24, fill: '#FF7A7A', weight: 700 })}</g>`).join('')}`;
  }

  return `<g transform="translate(${jx},${jy})">${base}${head(i === 0 ? 'ssh  prod-01' : i === 1 ? 'release.sh' : i === 2 ? 'systemctl' : i === 3 ? 'journalctl -f api' : 'deploy', i === 3 || i === 4 ? 'LIVE' : 'PROD')}${body}</g>
  ${flash > 0 ? `<rect width="${W}" height="${H}" fill="#fff" opacity="${flash * 0.12}"/>` : ''}
  ${[0, 1, 2].map((k) => `<rect x="0" y="${(T * 310 + k * 640) % H}" width="${W}" height="2" fill="#fff" opacity="0.035"/>`).join('')}`;
}

function headline1(T) {
  const p1 = S(T, 0.18, 0.62), p2 = S(T, 0.42, 0.9);
  const red = S(T, 2.9, 3.15) * 0.25;
  return `<g>
    ${txt('Still deploying', 540, 1150, { size: 96, weight: 700, ls: -2, op: p1 })}
    ${txt('with SSH scripts?', 540, 1260, { size: 96, weight: 700, ls: -2, op: p2, fill: WHITE })}
    <rect x="${540 - 400 * p2}" y="1292" width="${800 * p2}" height="5" rx="3" fill="${RED}" opacity="${0.9 * p2}"/>
    ${txt('●  PROD  2:14 AM  ·  release in progress', 540, 1070, { family: MONO, size: 24, fill: '#A57878', op: p1 * 0.95, ls: 2 })}
  </g>`;
}

// ---------- Scene 2: 4-8s TRANSFORMATION ----------
function scene2(t, T) {
  let s = '';
  // frozen error card collapsing
  const fx = 1 - easeIn(S(t, 0.2, 0.8));
  if (fx > 0.01) {
    s += `<g opacity="${fx}">
      ${[0, 1, 2, 3, 4, 5, 6, 7].map((k) => {
        const gy = 330 + k * 106;
        const off = Math.sin(k * 2.3 + t * 20) * 40 * (1 - S(t, 0, 0.5));
        const hot = k === 2 || k === 4;
        return `<rect x="${-40 + off}" y="${gy}" width="1160" height="98" fill="${hot ? '#1A1214' : '#0E1418'}" stroke="${hot ? '#5A2E2E' : '#223038'}"/>
        ${txt(['release.sh', 'systemctl restart api', 'address already in use', '502 bad gateway', 'rollback: none'][k % 5], 90, gy + 62, { family: MONO, size: 30, fill: hot ? '#FF8A8A' : '#6D8086', anchor: 'start' })}`;
      }).join('')}
      <rect width="${W}" height="${H}" fill="#fff" opacity="${0.06 * (1 - S(t, 0, 0.3))}"/>
    </g>`;
  }
  // converging particle stream into a line
  const cp = S(t, 0.15, 1.15);
  let parts = '';
  for (let k = 0; k < 46; k++) {
    const a = k * 2.399;
    const seed = ((k * 9301 + 49297) % 233280) / 233280;
    const r = (1 - cp) * (200 + seed * 520);
    const x = 540 + Math.cos(a) * r * 0.85;
    const y = 760 + Math.sin(a) * r;
    const o = cp > 0.02 ? (1 - cp) * 0.9 : 0;
    parts += dot(x, y, 2 + seed * 2.4, k % 3 ? CYAN : LIME, o);
  }
  s += parts;
  // horizontal beam grows
  const beam = S(t, 1.05, 1.85);
  if (beam > 0) {
    const len = 760 * easeOut(beam);
    s += `<rect x="${540 - len / 2}" y="754" width="${len}" height="7" rx="4" fill="url(#brandH)" opacity="${0.95 * (1 - S(t, 1.7, 2.3))}"/>
      <rect x="${540 - len / 2}" y="738" width="${len}" height="38" rx="19" fill="url(#brandH)" opacity="${0.28 * (1 - S(t, 1.7, 2.3))}" filter="url(#blur8)"/>`;
  }
  // logo reveal 5.2s
  const lp = S(t, 0.7, 1.5);
  const lsc = lerp(1.12, 1, easeOut((t - 0.7) / 1.5)) * (1 + 0.05 * S(t, 1.0, 4.0));
  const lrot = lerp(-6, 0, easeOut((t - 0.7) / 1.6));
  s += logoImg(540, 690, 320, lp, 0.7 + 0.3 * S(t, 1.5, 2.6), lrot, lsc);
  s += `<g opacity="${S(t, 1.8, 2.4)}">${txt('PHELIX', 540, 908, { size: 30, weight: 700, ls: 16, fill: '#9FB7B0' })}</g>`;
  // headline
  const h1 = S(t, 2.25, 2.7), h2 = S(t, 2.42, 2.9), h3 = S(t, 2.6, 3.1);
  s += `<g>
    ${txt('Your deployment', 540, 1130, { size: 74, weight: 700, ls: -1.5, op: h1 })}
    ${txt('workflow needs', 540, 1216, { size: 74, weight: 700, ls: -1.5, op: h2 })}
    ${txt('an upgrade.', 540, 1302, { size: 74, weight: 700, ls: -1.5, op: h3, fill: 'url(#brandH)' })}
  </g>`;
  // wide light sweep
  const sw = S(t, 2.9, 3.7);
  if (sw > 0 && sw < 1) s += `<rect x="${-400 + sw * 1900}" y="0" width="260" height="${H}" fill="url(#brandH)" opacity="0.05" transform="skewX(-12)"/>`;
  return s;
}

// ---------- Scene 3: 8-14s PRODUCT ----------
function scene3(t) {
  let s = '';
  const outTerm = 1 - S(t, 1.25, 1.6);
  if (outTerm > 0) {
    const ty = 400 - 40 * S(t, 1.15, 1.6);
    const cmd = '$ phelix build api --port 8080';
    const n = Math.ceil(cmd.length * clamp((t - 0.25) / 0.65));
    const cur = t < 1.0 && Math.floor(t * 12) % 2 === 0 ? '▌' : '';
    const lines = [
      ['✓ go toolchain 1.27', EMERALD, 0.95],
      ['✓ compiled  ./cmd/api', EMERALD, 1.25],
      ['→ builds/v5  (is_current=false)', CYAN, 1.55],
    ];
    s += `<g opacity="${outTerm}" filter="url(#shadow)">
      ${card(80, ty, 920, 420, '#23343C', 1, 26, '#0A1319')}
      <rect x="80" y="${ty}" width="920" height="62" rx="26" fill="#111C22"/><rect x="80" y="${ty + 36}" width="920" height="26" fill="#111C22"/>
      ${dot(116, ty + 32, 8, '#FF5F57')}${dot(142, ty + 32, 8, '#FEBC2E')}${dot(168, ty + 32, 8, '#28C840')}
      ${txt('phelix  build', 540, ty + 40, { family: MONO, size: 21, fill: '#5E747C' })}
      ${txt(cmd.slice(0, n) + cur, 128, ty + 138, { family: MONO, size: 33, fill: WHITE, anchor: 'start', weight: 600 })}
      ${lines.map(([l, c, at], k) => txt(l, 128, ty + 210 + k * 62, { family: MONO, size: 27, fill: c, anchor: 'start', op: S(t, at, at + 0.18) })).join('')}
    </g>`;
  }
  // header phrase
  const hp = S(t, 0.05, 0.4);
  s += `<g opacity="${hp}">${txt('Build. Deploy. Verify.', 540, 275, { size: 58, weight: 700, ls: -1 })}</g>`;
  if (t > 1.55) s += `<g opacity="${S(t, 1.55, 1.85)}"><rect x="${540 - 260 * S(t, 1.55, 1.95)}" y="303" width="${520 * S(t, 1.55, 1.95)}" height="4" rx="2" fill="url(#brandH)"/></g>`;

  // deployment panels
  const pp = S(t, 1.35, 1.8);
  if (pp > 0) {
    const switchP = S(t, 3.35, 3.95);
    const panels = [
      { x: 60, v: 'v4', col: EMERALD, port: ':9001', live: 1 - switchP, start: true },
      { x: 560, v: 'v5', col: CYAN, port: ':9002', live: switchP, start: false },
    ];
    for (const p of panels) {
      const y = 350 + (1 - easeOut((t - 1.35) / 0.6)) * 30;
      const isV5 = !p.start;
      const hc = isV5 ? S(t, 2.05, 2.85) : 1;
      const st = isV5 ? (hc >= 1 ? 'HEALTHY' : 'CHECKING') : (switchP > 0.5 ? 'DRAINED' : 'LIVE');
      s += `<g opacity="${pp}">
        ${card(p.x, y, 460, 372, p.col, 0.45 + 0.55 * p.live, 26, '#0A1319')}
        <rect x="${p.x}" y="${y}" width="460" height="6" rx="3" fill="${p.col}" opacity="${0.35 + 0.65 * p.live}"/>
        ${txt(p.v, p.x + 40, y + 92, { family: MONO, size: 60, weight: 700, fill: WHITE, anchor: 'start' })}
        ${txt(st, p.x + 424, y + 74, { family: MONO, size: 19, fill: p.col, anchor: 'end', weight: 600, ls: 1 })}
        ${txt('api  ' + p.port, p.x + 40, y + 142, { family: MONO, size: 23, fill: '#93A7AF', anchor: 'start' })}
        ${txt('/healthz', p.x + 40, y + 186, { family: MONO, size: 21, fill: '#71868E', anchor: 'start' })}
        ${isV5 ? `<g opacity="${hc >= 1 ? S(t, 2.8, 3.0) : 1}">
          ${hc < 1 ? `<circle cx="${p.x + 372}" cy="${y + 172}" r="24" fill="none" stroke="#26373F" stroke-width="6"/><circle cx="${p.x + 372}" cy="${y + 172}" r="24" fill="none" stroke="${CYAN}" stroke-width="6" stroke-linecap="round" stroke-dasharray="${151 * hc} 151" transform="rotate(-90 ${p.x + 372} ${y + 172})"/>` : check(p.x + 372, y + 172, 0.95, LIME, 1)}
        </g>` : ''}
        ${txt(p.live > 0.5 ? 'serving traffic' : p.live > 0 ? 'handing off' : 'warm, no traffic', p.x + 40, y + 250, { family: MONO, size: 21, fill: '#8FA3AB', anchor: 'start' })}
        <rect x="${p.x + 40}" y="${y + 280}" width="380" height="8" rx="4" fill="#1A272E"/>
        <rect x="${p.x + 40}" y="${y + 280}" width="${380 * p.live}" height="8" rx="4" fill="url(#brandH)" opacity="0.9"/>
        ${txt('traffic', p.x + 40, y + 326, { family: MONO, size: 18, fill: '#5E727A', anchor: 'start' })}
        ${txt(Math.round(p.live * 100) + '%', p.x + 420, y + 326, { family: MONO, size: 18, fill: p.live > 0.5 ? p.col : '#5E727A', anchor: 'end' })}
      </g>`;
    }
    // proxy node below panels
    const py = 830;
    s += `<g opacity="${S(t, 1.7, 2.05)}">
      ${card(384, py, 312, 92, '#2C414B', 1, 22, '#0B1620')}
      ${txt('phelix proxy', 540, py + 44, { size: 29, weight: 600, fill: '#DCEAE7' })}
      ${txt(':8080 public', 540, py + 76, { family: MONO, size: 18, fill: '#71868E' })}
    </g>`;
    const sx = lerp(290, 790, switchP);
    s += `<g opacity="${S(t, 1.9, 2.2)}">
      <path d="M540 ${py - 4} L${sx} 722" stroke="url(#brandH)" stroke-width="4" stroke-dasharray="10 8" stroke-dashoffset="${-t * 70}" opacity="0.9"/>
    </g>`;
    const fl = Math.max(0, 1 - Math.abs(t - 3.55) / 0.22);
    if (fl > 0) s += `<rect x="60" y="350" width="960" height="372" rx="26" fill="${LIME}" opacity="${fl * 0.06}"/>`;
  }

  const z = S(t, 3.5, 3.85);
  s += `<g opacity="${z}">
    ${txt('Zero-Downtime Deployments', 540, 1100, { size: 66, weight: 700, ls: -1.5 })}
    ${txt('proxy target swaps atomically, old slot stays warm', 540, 1158, { size: 27, fill: MUTED })}
    ${txt('BUILD  ·  DEPLOY  ·  VERIFY', 540, 1232, { family: MONO, size: 20, fill: '#7E969E', ls: 5, op: S(t, 3.9, 4.2) })}
  </g>`;
  return s;
}

// ---------- Scene 4: 14-20s OBSERVE ----------
function spark(x, y, w, h, seed, t, col) {
  let d = '';
  const n = 34;
  for (let i = 0; i < n; i++) {
    const v = 0.5 + 0.32 * Math.sin(i * 0.6 + seed) * Math.sin(i * 0.17 + seed * 2) + 0.12 * Math.sin(t * 2 + i * 0.5 + seed);
    const px = x + (i / (n - 1)) * w, py = y + h - clamp(v) * h;
    d += (i ? 'L' : 'M') + px + ' ' + py;
  }
  return `<path d="${d}" fill="none" stroke="${col}" stroke-width="2.5" opacity="0.85"/>
    <path d="${d} L${x + w} ${y + h} L${x} ${y + h} Z" fill="${col}" opacity="0.07"/>`;
}

function scene4(t) {
  let s = '';
  const pp = S(t, 0.05, 0.45);
  const rps = Math.round(1240 + Math.sin(t * 2.2) * 90 + Math.sin(t * 7) * 18);
  const live = 1 - S(t, 2.85, 3.25); // live dashboard fades out at rollback beat

  if (live > 0.01) {
    const dy = 340;
    s += `<g opacity="${pp * live}" transform="translate(0,${(1 - easeOut((t - 0.05) / 0.6)) * 26})">
      ${card(56, dy, 968, 560, '#243641', 1, 30, '#080F14')}
      <rect x="56" y="${dy}" width="968" height="84" rx="30" fill="#0D1720"/><rect x="56" y="${dy + 54}" width="968" height="30" fill="#0D1720"/>
      ${dot(96, dy + 42, 9, EMERALD)}${txt('api  ·  production', 124, dy + 51, { size: 27, weight: 600, anchor: 'start', fill: '#DCEAE7' })}
      ${txt('phelix monitor', 986, dy + 50, { family: MONO, size: 19, fill: '#6C828B', anchor: 'end' })}
      ${txt('RUNNING', 96, dy + 138, { family: MONO, size: 21, fill: EMERALD, anchor: 'start', weight: 700, ls: 2 })}
      ${txt('v5', 96, dy + 212, { family: MONO, size: 60, weight: 700, fill: WHITE, anchor: 'start' })}
      ${txt('current build', 196, dy + 208, { size: 21, fill: MUTED, anchor: 'start' })}
      <line x1="370" y1="${dy + 112}" x2="370" y2="${dy + 262}" stroke="#22323B"/>
      ${txt('UPTIME', 398, dy + 138, { family: MONO, size: 19, fill: '#6C828B', anchor: 'start', ls: 2 })}
      ${txt('2d 04h', 398, dy + 188, { family: MONO, size: 38, weight: 700, fill: '#DCEAE7', anchor: 'start' })}
      <line x1="610" y1="${dy + 112}" x2="610" y2="${dy + 262}" stroke="#22323B"/>
      ${txt('REQ / S', 638, dy + 138, { family: MONO, size: 19, fill: '#6C828B', anchor: 'start', ls: 2 })}
      ${txt(String(rps), 638, dy + 188, { family: MONO, size: 38, weight: 700, fill: LIME, anchor: 'start' })}
      <line x1="900" y1="${dy + 112}" x2="900" y2="${dy + 262}" stroke="#22323B"/>
      ${txt('HEALTH', 890, dy + 138, { family: MONO, size: 19, fill: '#6C828B', anchor: 'end', ls: 2 })}
      ${txt('2/2 ok', 890, dy + 188, { family: MONO, size: 32, weight: 700, fill: EMERALD, anchor: 'end' })}
      ${txt('CPU', 96, dy + 320, { family: MONO, size: 18, fill: '#6C828B', anchor: 'start', ls: 1 })}
      ${txt('18%', 96, dy + 358, { family: MONO, size: 32, weight: 700, fill: '#DCEAE7', anchor: 'start' })}
      ${spark(96, dy + 382, 420, 76, 1, t, CYAN)}
      ${txt('MEMORY', 564, dy + 320, { family: MONO, size: 18, fill: '#6C828B', anchor: 'start', ls: 1 })}
      ${txt('246 MB', 564, dy + 358, { family: MONO, size: 32, weight: 700, fill: '#DCEAE7', anchor: 'start' })}
      ${spark(564, dy + 382, 420, 76, 3.4, t, EMERALD)}
      ${txt('2 instances  ·  1 public port  ·  proxy active', 96, dy + 508, { family: MONO, size: 20, fill: '#7B8F98', anchor: 'start' })}
    </g>`;
    // deployment history rows, below card
    s += `<g opacity="${pp * live}">
      ${txt('DEPLOYMENTS', 96, 948, { family: MONO, size: 18, fill: '#5E727A', anchor: 'start', ls: 2 })}
    </g>`;
    [['v5', 'current', LIME], ['v4', 'served 2d', '#9DB0B8'], ['v3', 'served 5d', '#9DB0B8'], ['v2', 'rolled back', '#C99A5B'], ['v1', 'served 9d', '#9DB0B8']].forEach(([v, st, c], k) => {
      const at = 1.35 + k * 0.08;
      const op = S(t, at, at + 0.3) * live;
      if (op < 0.01) return;
      s += `<g opacity="${op}">
        <rect x="56" y="${972 + k * 56}" width="968" height="50" fill="${k % 2 ? '#0A1218' : '#0A1015'}" stroke="#16232B" stroke-opacity=".6"/>
        ${txt(v, 96, 1005 + k * 56, { family: MONO, size: 24, weight: 700, fill: WHITE, anchor: 'start' })}
        ${txt(st, 190, 1005 + k * 56, { family: MONO, size: 20, fill: k === 0 ? LIME : '#7B8F98', anchor: 'start' })}
        ${k === 0 ? dot(972, 998 + k * 56, 6, LIME) : ''}
      </g>`;
    });
  }

  // phase 2: version timeline + controlled rollback
  const rp = S(t, 2.85, 3.2);
  if (rp > 0) {
    s += `<g opacity="${rp}">
      ${card(56, 340, 968, 340, '#243641', 1, 30, '#080F14')}
      <rect x="56" y="340" width="968" height="76" rx="30" fill="#0D1720"/><rect x="56" y="386" width="968" height="30" fill="#0D1720"/>
      ${txt('VERSION HISTORY', 96, 388, { family: MONO, size: 19, fill: '#7C919A', anchor: 'start', ls: 2 })}
      ${txt('retained builds on disk', 984, 388, { family: MONO, size: 18, fill: '#5E727A', anchor: 'end' })}
    </g>`;
    const sel = S(t, 3.5, 3.75);
    for (let k = 0; k < 5; k++) {
      const v = k + 1, x = 112 + k * 172;
      const isCur = v === 5, isTo = v === 4;
      const hi = isCur ? 1 - sel * 0.6 : isTo ? sel : 0;
      const col = isTo && sel > 0.4 ? LIME : isCur && sel < 0.6 ? CYAN : '#2A3B45';
      s += `<g opacity="${rp}">
        ${txt('v' + v, x + 46, 480, { family: MONO, size: 28, weight: 700, fill: hi > 0.3 ? WHITE : '#7B8F98' })}
        <rect x="${x}" y="504" width="112" height="76" rx="16" fill="${hi > 0.3 ? col : '#0C151C'}" fill-opacity="${0.14 + 0.18 * hi}" stroke="${col}" stroke-opacity="${0.45 + 0.5 * hi}"/>
        ${txt(v === 5 ? 'current' : v === 4 ? (sel > 0.4 ? 'target' : 'ready') : 'retained', x + 56, 548, { family: MONO, size: 15, fill: hi > 0.3 ? col : '#5E727A' })}
        ${isCur && sel > 0.4 ? `<line x1="${x + 14}" y1="542" x2="${x + 98}" y2="542" stroke="#FF6B6B" stroke-width="3" opacity="${sel * 0.85}"/>` : ''}
        ${k < 4 ? `<line x1="${x + 126}" y1="542" x2="${x + 158}" y2="542" stroke="#24343E" stroke-width="2"/>` : ''}
      </g>`;
    }
    const mx = lerp(112 + 4 * 172, 112 + 3 * 172, S(t, 3.7, 4.05));
    s += `<g opacity="${rp * sel}">
      <path d="M${mx + 56} 452 L${mx + 56} 424" stroke="${LIME}" stroke-width="3"/>
      ${dot(mx + 56, 452, 5, LIME)}
      ${txt('rollback --to v4', mx + 56, 410, { family: MONO, size: 18, fill: LIME })}
    </g>`;
    // mount the switching card directly under the timeline
    const cfl = S(t, 4.1, 4.4);
    s += `<g opacity="${cfl}">
      ${card(56, 852, 968, 268, '#243641', 1, 28, '#080F14')}
      ${txt('SWITCHING BACK', 96, 910, { family: MONO, size: 18, fill: '#7C919A', anchor: 'start', ls: 2 })}
      ${txt('traffic  →  v4', 96, 970, { family: MONO, size: 30, fill: LIME, anchor: 'start' })}
      ${txt('binary  →  builds/v4', 96, 1020, { family: MONO, size: 30, fill: CYAN, anchor: 'start' })}
      ${txt('env  →  env/v4.enc', 96, 1070, { family: MONO, size: 30, fill: EMERALD, anchor: 'start' })}
      <rect x="640" y="892" width="336" height="152" rx="22" fill="#0B1512" stroke="${EMERALD}" stroke-opacity=".45"/>
      ${check(700, 954, 1, LIME, cfl)}
      ${txt('rollback complete', 808, 956, { size: 24, weight: 600, fill: '#DCEAE7', anchor: 'start' })}
      ${txt('no downtime', 808, 996, { family: MONO, size: 19, fill: EMERALD, anchor: 'start' })}
    </g>`;
  }

  // headlines
  const m1 = win(t, 1.15, 3.3, 0.2, 0.2);
  s += `<g opacity="${m1}">${txt('Monitor Everything.', 540, 1350, { size: 76, weight: 700, ls: -1.5 })}</g>`;
  const m2 = S(t, 3.3, 3.7);
  s += `<g opacity="${m2}">
    ${txt('Roll Back with Confidence.', 540, 1290, { size: 58, weight: 700, ls: -1 })}
    ${txt('controlled, deliberate, one command', 540, 1348, { size: 26, fill: MUTED, op: S(t, 4.1, 4.4) })}
  </g>`;
  return s;
}

// ---------- Scene 5: 20-28s BRAND ----------
function scene5(t) {
  let s = '';
  // UI collapse
  const cp = 1 - S(t, 0.0, 0.55);
  if (cp > 0.01) {
    const k = 1 - 0.22 * S(t, 0, 0.5);
    s += `<g opacity="${cp * 0.8}" transform="translate(540,760) scale(${k}) translate(-540,-760)">
      <rect x="56" y="300" width="968" height="560" rx="30" fill="none" stroke="#2A3D47" stroke-width="2"/>
      <rect x="120" y="390" width="840" height="140" rx="20" fill="none" stroke="#22333C"/>
      <rect x="120" y="560" width="400" height="180" rx="20" fill="none" stroke="#22333C"/>
      <rect x="560" y="560" width="400" height="180" rx="20" fill="none" stroke="#22333C"/>
    </g>`;
  }
  // light wave
  const wv = S(t, 0.1, 1.0);
  const wv2 = S(t, 4.45, 5.55);
  const waveAt = (p, gain) => p > 0 && p < 1 ? `<g opacity="${gain}">
      <rect x="0" y="${lerp(1500, 240, p) - 2}" width="${W}" height="5" fill="url(#brandH)" opacity="0.85"/>
      <rect x="0" y="${lerp(1500, 240, p) - 55}" width="${W}" height="110" fill="url(#brandH)" opacity="0.12" filter="url(#blur20)"/></g>` : '';
  // logo + masked sheen
  const lp = S(t, 0.35, 1.0);
  const push = 1 + 0.02 * S(t, 0.4, 5.5);
  const lh = 330;
  const lw = lh * LAR;
  const lx = 540, ly = 690;
  // slow continuous camera push across the whole scene
  const cam = 1 + 0.045 * S(t, 0.2, 8);
  s += `<g transform="translate(${lx},${ly}) scale(${cam}) translate(${-lx},${-ly})">`;
  s += `<g opacity="${lp}" transform="translate(${lx},${ly}) scale(${push})">
    <ellipse cx="0" cy="${lh * 0.08}" rx="${lw * 0.78}" ry="${lh * 0.56}" fill="url(#logoGlow)" opacity="0.55"/>
    <image href="data:image/png;base64,${LOGO}" x="${-lw / 2}" y="${-lh / 2}" width="${lw}" height="${lh}"/>
  </g>`;
  // sheen clipped to logo silhouette
  const sh = S(t, 1.2, 2.4);
  if (sh > 0 && sh < 1) {
    s += `<g mask="url(#logoMask)" transform="translate(${lx - lw / 2},${ly - lh / 2}) scale(${lw / 892})" opacity="0.55">
      <rect x="${-700 + sh * 1900}" y="-200" width="300" height="1500" fill="#fff" opacity="0.55" transform="skewX(-18)"/>
    </g>`;
  }
  // brand name
  s += `<g opacity="${S(t, 1.35, 1.85)}">${txt('PHELIX', 540, 905, { size: 30, weight: 700, ls: 20, fill: '#A9BCB7' })}</g>`;
  // headline lines
  const a = S(t, 1.7, 2.1), b = S(t, 2.25, 2.65);
  s += `<g>
    ${txt('YOUR SERVERS. YOUR STACK.', 540, 1070, { size: 57, weight: 700, ls: 0.5, op: a })}
    ${txt('ONE OPERATIONAL LAYER.', 540, 1155, { size: 57, weight: 700, ls: 0.5, op: b, fill: 'url(#brandH)' })}
  </g>`;
  // url pill
  const up = S(t, 3.55, 4.0);
  s += `<g opacity="${up}" transform="translate(0,${(1 - easeOut((t - 3.55) / 0.6)) * 16})">
    <rect x="270" y="1290" width="540" height="108" rx="54" fill="#0A1218" stroke="${LIME}" stroke-opacity=".45"/>
    <rect x="270" y="1290" width="540" height="108" rx="54" fill="url(#brandH)" opacity="0.06"/>
    ${txt('phelix.anophel.com', 540, 1360, { size: 43, weight: 600, ls: 0.5 })}
  </g>`;
  s += `<g opacity="${S(t, 4.05, 4.4)}">${txt('single binary  ·  Go + Rust  ·  your own servers', 540, 1462, { size: 26, fill: MUTED })}</g>`;
  // gentle final glow pulse
  s += `<circle cx="540" cy="690" r="${420 + 10 * Math.sin(t * 1.4)}" fill="url(#logoGlow)" opacity="${0.14 * lp}" filter="url(#blur40)"/>`;
  // second light pass so the hold breathes
  return s + '</g>' + waveAt(wv, 1) + waveAt(wv2, 0.55);
}

const TL = [
  [0.0, 4.0, 'pain'],
  [4.0, 8.0, 'transform'],
  [8.0, 14.0, 'product'],
  [14.0, 20.0, 'observe'],
  [20.0, 28.0, 'brand'],
];

function frameAt(T) {
  let body = '';
  // scene-specific background, crossfaded through the shared bg
  const sc = TL.find(([a, b]) => T >= a && T < b) || TL[TL.length - 1];
  const [a, b, name] = sc;
  const local = T - a;

  if (name === 'pain') body += term1(T, T) + headline1(T);
  if (name === 'transform') body += scene2(local, T);
  if (name === 'product') body += scene3(local);
  if (name === 'observe') body += scene4(local);
  if (name === 'brand') body += scene5(local);

  // scene fades at boundaries
  let op = 1;
  if (name === 'pain') op = 1 - S(T, 3.82, 4.0);
  if (name === 'transform') op = S(T, 4.0, 4.22) * (1 - S(T, 7.78, 8.0));
  if (name === 'product') op = S(T, 8.0, 8.2) * (1 - S(T, 13.78, 14.0));
  if (name === 'observe') op = S(T, 14.0, 14.2) * (1 - S(T, 19.78, 20.0));
  if (name === 'brand') op = S(T, 20.0, 20.25);
  body = `<g opacity="${op}">${body}</g>`;

  const red = name === 'pain' ? 0.5 + 0.2 * Math.sin(T * 3) : 0;
  const bright = name === 'brand' ? 1 : name === 'observe' || name === 'product' ? 0.8 : 0.3;
  const grain = 0.05;

  return `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}">
    ${DEFS}${bg(T, { red, bright })}${body}
    <rect width="${W}" height="${H}" fill="url(#vig)"/>
    <rect width="${W}" height="${H}" filter="url(#grain)" opacity="${grain}" style="mix-blend-mode:overlay"/>
  </svg>`;
}

const only = process.argv[2] != null ? parseFloat(process.argv[2]) : null;
fs.mkdirSync(DIR + '/frames', { recursive: true });
if (only != null) {
  fs.writeFileSync(`${DIR}/preview_${String(only.toFixed(2)).padStart(5, '0')}.svg`, frameAt(only));
  console.log('preview at', only);
} else {
  const N = FPS * DUR;
  for (let i = 0; i < N; i++) fs.writeFileSync(`${DIR}/frames/f_${String(i).padStart(4, '0')}.svg`, frameAt(i / FPS));
  console.log('wrote', N, 'svgs');
}
