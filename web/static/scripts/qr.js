// web/static/scripts/qr.js
// QR codes in the browser: byte mode, error correction level L, versions 1 to 20 (up to 858 bytes). It is the same algorithm as
// internal/qr (Go), whose tests read the codes back with an independent decoder; the two are held to the same output (docs/vpn.md).
// Used to show a device's WireGuard file as a code: the file never leaves the page.

window.QR = (function () {
    const ECC = [0, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28];
    const BLOCKS = [0, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8];
    const MAX_VERSION = 20;

    const sizeOf = (v) => v * 4 + 17;
    const countBits = (v) => (v >= 10 ? 16 : 8);

    function rawCodewords(v) {
        let r = (16 * v + 128) * v + 64;
        if (v >= 2) {
            const n = Math.floor(v / 7) + 2;
            r -= (25 * n - 10) * n - 55;
            if (v >= 7) r -= 36;
        }
        return Math.floor(r / 8);
    }
    const dataCodewordsOf = (v) => rawCodewords(v) - ECC[v] * BLOCKS[v];
    const capacity = (v) => Math.floor((dataCodewordsOf(v) * 8 - 4 - countBits(v)) / 8);

    function gfMul(x, y) {
        let z = 0;
        for (let i = 7; i >= 0; i--) {
            z = (z << 1) ^ ((z >> 7) * 0x11D);
            z ^= ((y >> i) & 1) * x;
        }
        return z;
    }
    function rsDivisor(degree) {
        const res = new Array(degree).fill(0);
        res[degree - 1] = 1;
        let root = 1;
        for (let i = 0; i < degree; i++) {
            for (let j = 0; j < degree; j++) {
                res[j] = gfMul(res[j], root);
                if (j + 1 < degree) res[j] ^= res[j + 1];
            }
            root = gfMul(root, 2);
        }
        return res;
    }
    function rsRemainder(data, divisor) {
        const res = new Array(divisor.length).fill(0);
        for (const b of data) {
            const factor = b ^ res[0];
            res.shift();
            res.push(0);
            divisor.forEach((c, i) => { res[i] ^= gfMul(c, factor); });
        }
        return res;
    }

    function dataCodewords(bytes, v) {
        const bits = [];
        const put = (val, n) => { for (let i = n - 1; i >= 0; i--) bits.push((val >> i) & 1); };
        put(4, 4);
        put(bytes.length, countBits(v));
        bytes.forEach(b => put(b, 8));
        const capBits = dataCodewordsOf(v) * 8;
        for (let t = 0; t < 4 && bits.length < capBits; t++) bits.push(0);
        while (bits.length % 8 !== 0) bits.push(0);
        const out = [];
        for (let i = 0; i < bits.length; i += 8) {
            let x = 0;
            for (let j = 0; j < 8; j++) x = (x << 1) | bits[i + j];
            out.push(x);
        }
        for (let pad = 0xEC; out.length < dataCodewordsOf(v); pad ^= 0xEC ^ 0x11) out.push(pad);
        return out;
    }

    function interleave(data, v) {
        const nb = BLOCKS[v], ecc = ECC[v], raw = rawCodewords(v);
        const short = nb - (raw % nb);
        const shortLen = Math.floor(raw / nb);
        const div = rsDivisor(ecc);
        const blocks = [];
        let k = 0;
        for (let i = 0; i < nb; i++) {
            const l = shortLen - ecc + (i >= short ? 1 : 0);
            const d = data.slice(k, k + l);
            k += l;
            const block = d.slice();
            if (i < short) block.push(0); // a hole where the longer blocks have their extra data byte
            rsRemainder(d, div).forEach(x => block.push(x));
            blocks.push(block);
        }
        const out = [];
        for (let i = 0; i < blocks[0].length; i++) {
            blocks.forEach((b, j) => { if (i !== shortLen - ecc || j >= short) out.push(b[i]); });
        }
        return out;
    }

    const bit = (x, i) => ((x >> i) & 1) !== 0;

    function Matrix(v) {
        const n = sizeOf(v);
        this.n = n;
        this.mod = Array.from({ length: n }, () => new Array(n).fill(false));
        this.fn = Array.from({ length: n }, () => new Array(n).fill(false));
    }
    Matrix.prototype.set = function (x, y, dark) { this.mod[y][x] = dark; this.fn[y][x] = true; };
    Matrix.prototype.finder = function (cx, cy) {
        for (let dy = -4; dy <= 4; dy++) {
            for (let dx = -4; dx <= 4; dx++) {
                const x = cx + dx, y = cy + dy;
                if (x >= 0 && x < this.n && y >= 0 && y < this.n) {
                    const d = Math.max(Math.abs(dx), Math.abs(dy));
                    this.set(x, y, d !== 2 && d !== 4);
                }
            }
        }
    };
    Matrix.prototype.alignment = function (cx, cy) {
        for (let dy = -2; dy <= 2; dy++) for (let dx = -2; dx <= 2; dx++) this.set(cx + dx, cy + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1);
    };
    function alignmentPositions(v) {
        if (v === 1) return [];
        const n = Math.floor(v / 7) + 2;
        const step = Math.floor((v * 4 + n * 2 + 1) / (n * 2 - 2)) * 2;
        const res = new Array(n).fill(0);
        res[0] = 6;
        for (let i = n - 1, pos = sizeOf(v) - 7; i >= 1; i--, pos -= step) res[i] = pos;
        return res;
    }
    function formatWord(mask) {
        const data = (1 << 3) | mask;
        let rem = data;
        for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >> 9) * 0x537);
        return ((data << 10) | rem) ^ 0x5412;
    }
    function versionWord(v) {
        let rem = v;
        for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >> 11) * 0x1F25);
        return (v << 12) | rem;
    }
    Matrix.prototype.formatBits = function (mask) {
        const bits = formatWord(mask), n = this.n;
        for (let i = 0; i <= 5; i++) this.set(8, i, bit(bits, i));
        this.set(8, 7, bit(bits, 6));
        this.set(8, 8, bit(bits, 7));
        this.set(7, 8, bit(bits, 8));
        for (let i = 9; i < 15; i++) this.set(14 - i, 8, bit(bits, i));
        for (let i = 0; i < 8; i++) this.set(n - 1 - i, 8, bit(bits, i));
        for (let i = 8; i < 15; i++) this.set(8, n - 15 + i, bit(bits, i));
        this.set(8, n - 8, true);
    };
    Matrix.prototype.versionBits = function (v) {
        if (v < 7) return;
        const bits = versionWord(v), n = this.n;
        for (let i = 0; i < 18; i++) {
            const a = n - 11 + (i % 3), b = Math.floor(i / 3);
            this.set(a, b, bit(bits, i));
            this.set(b, a, bit(bits, i));
        }
    };
    Matrix.prototype.functionPatterns = function (v) {
        const n = this.n;
        for (let i = 0; i < n; i++) { this.set(6, i, i % 2 === 0); this.set(i, 6, i % 2 === 0); }
        this.finder(3, 3);
        this.finder(n - 4, 3);
        this.finder(3, n - 4);
        const pos = alignmentPositions(v);
        pos.forEach((x, i) => pos.forEach((y, j) => {
            if ((i === 0 && j === 0) || (i === 0 && j === pos.length - 1) || (i === pos.length - 1 && j === 0)) return;
            this.alignment(x, y);
        }));
        this.formatBits(0); // reserves the places
        this.versionBits(v);
    };
    Matrix.prototype.place = function (data) {
        const n = this.n;
        let i = 0;
        for (let right = n - 1; right >= 1; right -= 2) {
            if (right === 6) right = 5;
            for (let vert = 0; vert < n; vert++) {
                for (let j = 0; j < 2; j++) {
                    const x = right - j;
                    const upward = ((right + 1) & 2) === 0;
                    const y = upward ? n - 1 - vert : vert;
                    if (!this.fn[y][x] && i < data.length * 8) {
                        this.mod[y][x] = bit(data[i >> 3], 7 - (i & 7));
                        i++;
                    }
                }
            }
        }
    };
    function maskBit(mask, x, y) {
        switch (mask) {
            case 0: return (x + y) % 2 === 0;
            case 1: return y % 2 === 0;
            case 2: return x % 3 === 0;
            case 3: return (x + y) % 3 === 0;
            case 4: return (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0;
            case 5: return ((x * y) % 2) + ((x * y) % 3) === 0;
            case 6: return ((((x * y) % 2) + ((x * y) % 3)) % 2) === 0;
            default: return ((((x + y) % 2) + ((x * y) % 3)) % 2) === 0;
        }
    }
    Matrix.prototype.applyMask = function (mask) {
        for (let y = 0; y < this.n; y++) for (let x = 0; x < this.n; x++) if (!this.fn[y][x] && maskBit(mask, x, y)) this.mod[y][x] = !this.mod[y][x];
    };
    Matrix.prototype.penalty = function () {
        const n = this.n, mod = this.mod;
        let score = 0;
        const line = (get) => {
            let run = 1;
            for (let i = 1; i < n; i++) {
                if (get(i) === get(i - 1)) {
                    run++;
                    if (run === 5) score += 3; else if (run > 5) score++;
                } else run = 1;
            }
            const pat = (p) => {
                for (let s = 0; s + p.length <= n; s++) {
                    let ok = true;
                    for (let k = 0; k < p.length; k++) if (get(s + k) !== (p[k] === '1')) { ok = false; break; }
                    if (ok) score += 40;
                }
            };
            pat('10111010000');
            pat('00001011101');
        };
        for (let y = 0; y < n; y++) line((i) => mod[y][i]);
        for (let x = 0; x < n; x++) line((i) => mod[i][x]);
        let dark = 0;
        for (let y = 0; y < n; y++) {
            for (let x = 0; x < n; x++) {
                if (mod[y][x]) dark++;
                if (x + 1 < n && y + 1 < n && mod[y][x] === mod[y][x + 1] && mod[y][x] === mod[y + 1][x] && mod[y][x] === mod[y + 1][x + 1]) score += 3;
            }
        }
        const total = n * n;
        const k = Math.floor((Math.abs(dark * 20 - total * 10) + total - 1) / total);
        return score + (k - 1) * 10;
    };

    // encode makes the code of a text (UTF-8): { modules: rows of booleans, version } or null when it does not fit.
    function encode(text) {
        const bytes = Array.from(new TextEncoder().encode(text));
        let ver = 0;
        for (let v = 1; v <= MAX_VERSION; v++) if (bytes.length <= capacity(v)) { ver = v; break; }
        if (!ver) return null;
        const words = interleave(dataCodewords(bytes, ver), ver);
        let best = null, bestScore = 0;
        for (let mask = 0; mask < 8; mask++) {
            const m = new Matrix(ver);
            m.functionPatterns(ver);
            m.place(words);
            m.applyMask(mask);
            m.formatBits(mask);
            const p = m.penalty();
            if (best === null || p < bestScore) { best = m; bestScore = p; }
        }
        return { modules: best.mod, version: ver };
    }

    // svg draws a code with its quiet zone of four modules; the colours are fixed, so that any scanner can read it on any theme.
    function svg(text, pixels) {
        const q = encode(text);
        if (!q) return null;
        const n = q.modules.length, total = n + 8;
        let path = '';
        q.modules.forEach((row, y) => {
            let x = 0;
            while (x < n) {
                if (!row[x]) { x++; continue; }
                let w = 1;
                while (x + w < n && row[x + w]) w++;
                path += `M${x + 4} ${y + 4}h${w}v1h-${w}z`;
                x += w;
            }
        });
        const px = pixels || 280;
        return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${total} ${total}" width="${px}" height="${px}" shape-rendering="crispEdges" role="img" aria-label="QR code"><rect width="${total}" height="${total}" fill="#fff"/><path d="${path}" fill="#000"/></svg>`;
    }

    return { encode, svg, capacity, MAX_BYTES: capacity(MAX_VERSION) };
})();
