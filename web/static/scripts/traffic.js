// web/static/scripts/traffic.js

window.TrafficModule = (() => {
    let statsTimerId = null;
    let graphTimerId = null;

    const interfaceStates = {};

    const RANGE_CONFIG = {
        '5m':  { labelFormat: 'time' },
        '1h':  { labelFormat: 'time' },
        '24h': { labelFormat: 'date-time' },
        '30d': { labelFormat: 'date' }
    };

    const RANGE_MS = {
        '5m':  5 * 60 * 1000,
        '1h':  60 * 60 * 1000,
        '24h': 24 * 60 * 60 * 1000,
        '30d': 30 * 24 * 60 * 60 * 1000
    };

    const PALETTE = {
        rxLine: '#00A86B',
        rxGradStart: 'rgba(0, 168, 107, 0.28)',
                        rxGradEnd: 'rgba(0, 168, 107, 0.01)',
                        txLine: '#0072CE',
                        txGradStart: 'rgba(0, 114, 206, 0.28)',
                        txGradEnd: 'rgba(0, 114, 206, 0.01)',
                        gridLine: 'rgba(255, 255, 255, 0.05)',
                        textMuted: '#64748B',
                        textMain: '#94A3B8',
                        crosshair: 'rgba(255, 255, 255, 0.25)',
                        cardBg: 'rgba(15, 23, 42, 0.95)',
                        cardBorder: '#334155'
    };

    async function safeFetch(url) {
        if (typeof apiFetch === 'function') return await apiFetch(url);
        try { return await fetch(url); } catch (e) { return null; }
    }

    function isPhysicalInterface(ifaceName, metaObj) {
        if (metaObj && typeof metaObj.is_physical === 'boolean') return metaObj.is_physical;
        if (ifaceName === 'lo' || ifaceName.includes('.')) return false;
        if (/^(veth|docker|br-|wg|tun|tap|bond|macvtap)/i.test(ifaceName)) return false;
        return true;
    }

    function formatBpsUnitFactor(maxBps) {
        if (!maxBps || maxBps < 1000) return { unit: 'bps', divisor: 1 };
        if (maxBps < 1000000) return { unit: 'Kbps', divisor: 1000 };
        if (maxBps < 1000000000) return { unit: 'Mbps', divisor: 1000000 };
        return { unit: 'Gbps', divisor: 1000000000 };
    }

    function formatBytes(bytes) {
        if (!bytes || bytes <= 0) return '0 B';
        const k = 1024;
        const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
        const i = Math.floor(Math.log(bytes) / Math.log(k));
        return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
    }

    function normalizeTimestamp(ts) {
        if (!ts) return Date.now();
        return ts < 1e11 ? ts * 1000 : ts;
    }

    function formatXTime(ts, mode) {
        const d = new Date(normalizeTimestamp(ts));
        if (mode === 'date') return `${d.getMonth() + 1}/${d.getDate()}`;
        if (mode === 'date-time') return `${d.getMonth() + 1}/${d.getDate()} ${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}`;
        return d.toLocaleTimeString([], { hour12: false, hour: '2-digit', minute: '2-digit' });
    }

    function drawSmoothPath(ctx, data, getX, getY) {
        if (data.length < 2) return;

        ctx.beginPath();
        ctx.moveTo(getX(0), getY(data[0]));

        for (let i = 0; i < data.length - 1; i++) {
            const p0X = getX(i);
            const p0Y = getY(data[i]);
            const p1X = getX(i + 1);
            const p1Y = getY(data[i + 1]);

            const midX = (p0X + p1X) / 2;
            const midY = (p0Y + p1Y) / 2;

            if (i === 0) {
                ctx.lineTo(midX, midY);
            } else {
                ctx.quadraticCurveTo(p0X, p0Y, midX, midY);
            }
        }

        const lastIdx = data.length - 1;
        ctx.lineTo(getX(lastIdx), getY(data[lastIdx]));
    }

    function drawInterfaceChart(iface) {
        const canvas = document.getElementById(`canvas-${iface}`);
        if (!canvas) return;
        const ctx = canvas.getContext('2d');

        const parentRect = canvas.parentElement ? canvas.parentElement.getBoundingClientRect() : { width: 0 };
        const displayWidth = parentRect.width || canvas.clientWidth || 800;

        canvas.width = displayWidth;
        canvas.height = 230;

        const width = canvas.width;
        const height = canvas.height;
        const padding = { top: 20, right: 25, bottom: 35, left: 65 };

        ctx.clearRect(0, 0, width, height);

        const state = interfaceStates[iface];
        if (!state) return;

        let rxData = (state.history.rx || []).map(b => b * 8);
        let txData = (state.history.tx || []).map(b => b * 8);
        let tsData = (state.history.timestamps || []).map(normalizeTimestamp);

        if (rxData.length === 1) {
            rxData = [rxData[0], rxData[0]];
            txData = [txData[0], txData[0]];
            tsData = [tsData[0] - 1000, tsData[0]];
        }

        if (!rxData || rxData.length < 2) {
            ctx.fillStyle = PALETTE.textMuted;
            ctx.font = '12px Inter, system-ui, sans-serif';
            ctx.textAlign = 'center';
            const msg = state.isFetching
            ? 'Fetching telemetry from database...'
            : `No historical data stored for [${state.range}] window yet.`;
            ctx.fillText(msg, width / 2, height / 2);
            return;
        }

        const maxVal = Math.max(...rxData, ...txData, 10000);
        const scale = formatBpsUnitFactor(maxVal);

        const graphWidth = width - padding.left - padding.right;
        const graphHeight = height - padding.top - padding.bottom;

        const now = Date.now();
        const rangeMs = RANGE_MS[state.range] || RANGE_MS['1h'];
        const maxTime = Math.max(now, tsData.length ? tsData[tsData.length - 1] : now);
        const minTime = maxTime - rangeMs;

        const getX = (idx) => {
            const ts = tsData[idx];
            if (!ts) return padding.left;
            const pct = (ts - minTime) / rangeMs;
            return padding.left + Math.max(0, Math.min(1, pct)) * graphWidth;
        };

        const getY = (val) => padding.top + graphHeight - (val / maxVal) * graphHeight;

        ctx.strokeStyle = PALETTE.gridLine;
        ctx.lineWidth = 1;
        ctx.fillStyle = PALETTE.textMain;
        ctx.font = '11px Inter, system-ui, sans-serif';
        ctx.textAlign = 'right';

        for (let i = 0; i <= 4; i++) {
            const y = padding.top + (graphHeight / 4) * i;
            ctx.beginPath();
            ctx.moveTo(padding.left, y);
            ctx.lineTo(width - padding.right, y);
            ctx.stroke();

            const labelVal = maxVal - (maxVal / 4) * i;
            ctx.fillText(`${(labelVal / scale.divisor).toFixed(1)} ${scale.unit}`, padding.left - 10, y + 4);
        }

        ctx.textAlign = 'center';
        const cfg = RANGE_CONFIG[state.range] || RANGE_CONFIG['1h'];
        const numTicks = 5;

        for (let i = 0; i < numTicks; i++) {
            const tickTime = minTime + (rangeMs / (numTicks - 1)) * i;
            const x = padding.left + (graphWidth / (numTicks - 1)) * i;

            ctx.beginPath();
            ctx.moveTo(x, padding.top + graphHeight);
            ctx.lineTo(x, padding.top + graphHeight + 4);
            ctx.strokeStyle = PALETTE.gridLine;
            ctx.stroke();

            ctx.fillStyle = PALETTE.textMain;
            ctx.fillText(formatXTime(tickTime, cfg.labelFormat), x, height - 10);
        }

        const rxGrad = ctx.createLinearGradient(0, padding.top, 0, padding.top + graphHeight);
        rxGrad.addColorStop(0, PALETTE.rxGradStart);
        rxGrad.addColorStop(1, PALETTE.rxGradEnd);

        drawSmoothPath(ctx, rxData, getX, getY);
        ctx.lineTo(getX(rxData.length - 1), padding.top + graphHeight);
        ctx.lineTo(getX(0), padding.top + graphHeight);
        ctx.closePath();
        ctx.fillStyle = rxGrad;
        ctx.fill();

        ctx.strokeStyle = PALETTE.rxLine;
        ctx.lineWidth = 2;
        drawSmoothPath(ctx, rxData, getX, getY);
        ctx.stroke();

        const txGrad = ctx.createLinearGradient(0, padding.top, 0, padding.top + graphHeight);
        txGrad.addColorStop(0, PALETTE.txGradStart);
        txGrad.addColorStop(1, PALETTE.txGradEnd);

        drawSmoothPath(ctx, txData, getX, getY);
        ctx.lineTo(getX(txData.length - 1), padding.top + graphHeight);
        ctx.lineTo(getX(0), padding.top + graphHeight);
        ctx.closePath();
        ctx.fillStyle = txGrad;
        ctx.fill();

        ctx.strokeStyle = PALETTE.txLine;
        ctx.lineWidth = 2;
        drawSmoothPath(ctx, txData, getX, getY);
        ctx.stroke();

        const hIdx = state.hoverIndex;
        if (hIdx >= 0 && hIdx < rxData.length) {
            const hX = getX(hIdx);
            const hRx = rxData[hIdx];
            const hTx = txData[hIdx];
            const hTs = tsData[hIdx];

            ctx.strokeStyle = PALETTE.crosshair;
            ctx.lineWidth = 1;
            ctx.setLineDash([3, 3]);
            ctx.beginPath();
            ctx.moveTo(hX, padding.top);
            ctx.lineTo(hX, padding.top + graphHeight);
            ctx.stroke();
            ctx.setLineDash([]);

            const drawDot = (x, y, color) => {
                ctx.fillStyle = color;
                ctx.beginPath();
                ctx.arc(x, y, 4.5, 0, Math.PI * 2);
                ctx.fill();
                ctx.strokeStyle = '#0F172A';
                ctx.lineWidth = 2;
                ctx.stroke();
            };

            drawDot(hX, getY(hRx), PALETTE.rxLine);
            drawDot(hX, getY(hTx), PALETTE.txLine);

            const rxStr = `${(hRx / scale.divisor).toFixed(2)} ${scale.unit}`;
            const txStr = `${(hTx / scale.divisor).toFixed(2)} ${scale.unit}`;
            const timeStr = formatXTime(hTs, 'date-time');

            const boxWidth = 160;
            const boxHeight = 68;
            let boxX = hX + 12;
            if (boxX + boxWidth > width - padding.right) boxX = hX - boxWidth - 12;

            ctx.fillStyle = PALETTE.cardBg;
            ctx.strokeStyle = PALETTE.cardBorder;
            ctx.lineWidth = 1;
            ctx.beginPath();
            ctx.roundRect(boxX, padding.top + 8, boxWidth, boxHeight, 6);
            ctx.fill();
            ctx.stroke();

            ctx.textAlign = 'left';
            ctx.fillStyle = PALETTE.textMuted;
            ctx.font = '10px Inter, system-ui, sans-serif';
            ctx.fillText(timeStr, boxX + 12, padding.top + 25);

            ctx.fillStyle = PALETTE.rxLine;
            ctx.beginPath();
            ctx.arc(boxX + 14, padding.top + 39, 3.5, 0, Math.PI * 2);
            ctx.fill();
            ctx.fillStyle = '#E2E8F0';
            ctx.font = '11px Inter, system-ui, sans-serif';
            ctx.fillText(`Inbound:  ${rxStr}`, boxX + 24, padding.top + 43);

            ctx.fillStyle = PALETTE.txLine;
            ctx.beginPath();
            ctx.arc(boxX + 14, padding.top + 55, 3.5, 0, Math.PI * 2);
            ctx.fill();
            ctx.fillStyle = '#E2E8F0';
            ctx.fillText(`Outbound: ${txStr}`, boxX + 24, padding.top + 59);
        }
    }

    function ensureGroupContainers() {
        const mainContainer = document.getElementById('traffic-interfaces-container');
        if (!mainContainer) return null;

        let wanSection = document.getElementById('traffic-wan-group');
        if (!wanSection) {
            wanSection = document.createElement('div');
            wanSection.id = 'traffic-wan-group';
            wanSection.className = 'traffic-section';
            wanSection.innerHTML = `
            <div class="traffic-section-header">
            <div class="traffic-section-icon wan">
            <svg class="traffic-svg-icon" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M21 12a9 9 0 01-9 9m9-9a9 9 0 00-9-9m9 9H3m9 9a9 9 0 01-9-9m9 9c1.657 0 3-4.03 3-9s-1.343-9-3-9m0 18c-1.657 0-3-4.03-3-9s1.343-9 3-9m-9 9a9 9 0 019-9"/>
            </svg>
            </div>
            <h2 class="traffic-section-title wan">WAN Interfaces</h2>
            <div class="traffic-header-line"></div>
            </div>
            <div id="wan-cards-wrapper" class="traffic-cards-wrapper"></div>
            `;
            mainContainer.appendChild(wanSection);
        }

        let lanSection = document.getElementById('traffic-lan-group');
        if (!lanSection) {
            lanSection = document.createElement('div');
            lanSection.id = 'traffic-lan-group';
            lanSection.className = 'traffic-section';
            lanSection.innerHTML = `
            <div class="traffic-section-header">
            <div class="traffic-section-icon lan">
            <svg class="traffic-svg-icon" fill="none" stroke="currentColor" viewBox="0 0 24 24">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10"/>
            </svg>
            </div>
            <h2 class="traffic-section-title lan">LAN Interfaces</h2>
            <div class="traffic-header-line"></div>
            </div>
            <div id="lan-cards-wrapper" class="traffic-cards-wrapper"></div>
            `;
            mainContainer.appendChild(lanSection);
        }

        return {
            wan: document.getElementById('wan-cards-wrapper'),
                        lan: document.getElementById('lan-cards-wrapper')
        };
    }

    function syncInterfaceCards(interfacesObj) {
        const groups = ensureGroupContainers();
        if (!groups) return;

        Object.entries(interfacesObj).forEach(([iface, meta]) => {
            if (!isPhysicalInterface(iface, meta)) return;

            const role = (meta.role || 'lan').toLowerCase();

            if (!interfaceStates[iface]) {
                interfaceStates[iface] = {
                    range: '1h',
                    role: role,
                    hoverIndex: -1,
                    isFetching: false,
                    history: { rx: [], tx: [], timestamps: [] }
                };

                const targetWrapper = role === 'wan' ? groups.wan : groups.lan;

                const card = document.createElement('div');
                card.className = 'traffic-interface-card';
                card.id = `traffic-card-${iface}`;
                card.innerHTML = `
                <div class="traffic-card-header">
                <div class="traffic-card-meta">
                <span class="traffic-badge ${role === 'wan' ? 'wan' : 'lan'}">
                ${iface.toUpperCase()}
                </span>
                <div class="traffic-stats-group">
                <span class="traffic-stat-item">
                <span class="traffic-dot rx"></span>
                <span class="traffic-stat-label">Inbound:</span>
                <strong id="rx-rate-${iface}" class="traffic-stat-value rx">0.00 bps</strong>
                </span>
                <span class="traffic-stat-item">
                <span class="traffic-dot tx"></span>
                <span class="traffic-stat-label">Outbound:</span>
                <strong id="tx-rate-${iface}" class="traffic-stat-value tx">0.00 bps</strong>
                </span>
                <span class="traffic-vol" id="vol-${iface}">Vol: 0 B</span>
                </div>
                </div>
                <div class="traffic-time-picker time-picker" data-iface="${iface}">
                <button class="time-btn" data-range="5m">5m</button>
                <button class="time-btn active" data-range="1h">1h</button>
                <button class="time-btn" data-range="24h">24h</button>
                <button class="time-btn" data-range="30d">30d</button>
                </div>
                </div>
                <div class="traffic-chart-container">
                <canvas id="canvas-${iface}" class="traffic-canvas"></canvas>
                </div>
                `;
                targetWrapper.appendChild(card);
                bindCardEvents(iface);

                requestAnimationFrame(() => {
                    fetchTrafficHistory(iface);
                });
            }
        });
    }

    function bindCardEvents(iface) {
        const card = document.getElementById(`traffic-card-${iface}`);
        if (!card) return;

        const canvas = card.querySelector(`#canvas-${iface}`);
        if (canvas) {
            canvas.addEventListener('mousemove', (e) => {
                const rect = canvas.getBoundingClientRect();
                const mouseX = e.clientX - rect.left;
                const paddingLeft = 65;
                const paddingRight = 25;

                const state = interfaceStates[iface];
                if (!state || !state.history.rx || state.history.rx.length < 1) return;

                const graphWidth = canvas.width - paddingLeft - paddingRight;

                if (mouseX >= paddingLeft && mouseX <= canvas.width - paddingRight) {
                    const now = Date.now();
                    const rangeMs = RANGE_MS[state.range] || RANGE_MS['1h'];
                    const tsData = (state.history.timestamps || []).map(normalizeTimestamp);
                    const maxTime = Math.max(now, tsData.length ? tsData[tsData.length - 1] : now);
                    const minTime = maxTime - rangeMs;

                    const mouseTime = minTime + ((mouseX - paddingLeft) / graphWidth) * rangeMs;

                    let closestIdx = -1;
                    let minDiff = Infinity;
                    for (let i = 0; i < tsData.length; i++) {
                        const diff = Math.abs(tsData[i] - mouseTime);
                        if (diff < minDiff) {
                            minDiff = diff;
                            closestIdx = i;
                        }
                    }
                    state.hoverIndex = closestIdx;
                } else {
                    state.hoverIndex = -1;
                }
                drawInterfaceChart(iface);
            });

            canvas.addEventListener('mouseleave', () => {
                const state = interfaceStates[iface];
                if (state) state.hoverIndex = -1;
                drawInterfaceChart(iface);
            });
        }

        const picker = card.querySelector('.time-picker');
        if (picker) {
            picker.addEventListener('click', (e) => {
                const btn = e.target.closest('.time-btn');
                if (!btn) return;

                picker.querySelectorAll('.time-btn').forEach(b => {
                    b.classList.remove('active');
                });
                btn.classList.add('active');

                const state = interfaceStates[iface];
                if (state) {
                    state.range = btn.dataset.range || '1h';
                    fetchTrafficHistory(iface);
                }
            });
        }
    }

    async function fetchTrafficHistory(iface) {
        const state = interfaceStates[iface];
        if (!state) return;

        state.isFetching = true;
        drawInterfaceChart(iface);

        const res = await safeFetch(`/api/v1/system/traffic?iface=${iface}&range=${state.range}`);
        state.isFetching = false;

        if (res && res.ok) {
            const data = await res.json();
            if (data && Array.isArray(data.rx) && Array.isArray(data.tx) && Array.isArray(data.timestamps)) {
                state.history = {
                    rx: data.rx,
                    tx: data.tx,
                    timestamps: data.timestamps
                };
            }
        }
        drawInterfaceChart(iface);
    }

    async function pollStats() {
        const res = await safeFetch('/api/v1/system/stats');
        if (!res || !res.ok) return;

        const data = await res.json();
        const interfacesObj = data.interfaces || {};

        syncInterfaceCards(interfacesObj);

        Object.entries(interfacesObj).forEach(([name, meta]) => {
            if (!interfaceStates[name]) return;

            const rxBps = (meta.rx_bytes_sec || 0) * 8;
            const txBps = (meta.tx_bytes_sec || 0) * 8;
            const totalBytes = (meta.rx_bytes || 0) + (meta.tx_bytes || 0);

            updateCardHeader(name, rxBps, txBps, totalBytes);
        });
    }

    function refreshAllGraphs() {
        Object.keys(interfaceStates).forEach(iface => fetchTrafficHistory(iface));
    }

    function updateCardHeader(iface, rxBps, txBps, totalBytes) {
        const rxEl = document.getElementById(`rx-rate-${iface}`);
        const txEl = document.getElementById(`tx-rate-${iface}`);
        const volEl = document.getElementById(`vol-${iface}`);

        const scale = formatBpsUnitFactor(Math.max(rxBps, txBps));

        if (rxEl) rxEl.innerText = `${(rxBps / scale.divisor).toFixed(2)} ${scale.unit}`;
        if (txEl) txEl.innerText = `${(txBps / scale.divisor).toFixed(2)} ${scale.unit}`;
        if (volEl) volEl.innerText = `Vol: ${formatBytes(totalBytes)}`;
    }

    return {
        init: () => {
            pollStats();

            statsTimerId = setInterval(pollStats, 1000);
            graphTimerId = setInterval(refreshAllGraphs, 10000);

            window.addEventListener('resize', () => {
                Object.keys(interfaceStates).forEach(iface => drawInterfaceChart(iface));
            });

            return () => {
                if (statsTimerId) clearInterval(statsTimerId);
                if (graphTimerId) clearInterval(graphTimerId);
            };
        }
    };
})();
