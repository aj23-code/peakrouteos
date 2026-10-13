// web/static/scripts/ap_home.js
// The access point's home page: the device itself, its place on the network (address, cable ports, wireless links), its Wi-Fi (how each
// adapter is doing and which networks it hosts) and the devices connected by Wi-Fi. docs/access-point.md.

window.APHomeModule = (function () {
    let timer = null;
    let wired = false;
    let status = null;
    let stats = null;
    let failed = false;
    let notYet = false;        // the settings of an access point are a draft: nothing is applied yet

    const $ = (id) => document.getElementById(id);
    const esc = (s) => escapeHtml(String(s == null ? '' : s));

    function uptimeText(sec) {
        if (!sec) return '–';
        const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
        return [d ? d + 'd' : '', h ? h + 'h' : '', m || (!d && !h) ? m + 'm' : ''].filter(Boolean).join(' ');
    }
    const agoText = (s) => (s >= 3600 ? Math.floor(s / 3600) + ' h ' + Math.floor((s % 3600) / 60) + ' min' : s >= 60 ? Math.floor(s / 60) + ' min' : s + ' s');
    const bytesText = (b) => (b >= 1073741824 ? (b / 1073741824).toFixed(1) + ' GB' : b >= 1048576 ? (b / 1048576).toFixed(1) + ' MB' : Math.max(0, Math.round(b / 1024)) + ' kB');

    // a signal reading in words: -30 to -55 excellent, to -67 good, to -75 fair, weaker poor
    function signalClass(dbm) { return dbm >= -67 ? 'ok' : dbm >= -75 ? 'warn' : 'bad'; }
    function signalWord(dbm) { return dbm >= -55 ? 'Excellent' : dbm >= -67 ? 'Good' : dbm >= -75 ? 'Fair' : 'Poor'; }
    const dot = (cls) => `<span class="apx-dot apx-dot--${cls}" aria-hidden="true"></span>`;

    // ---------------------------------------------------------------- cards

    function deviceCard() {
        if (!stats) return '';
        const mem = stats.memory_total_mb ? Math.round(stats.memory_used_mb * 100 / stats.memory_total_mb) : 0;
        const temp = stats.cpu_temp_celsius == null ? '–' : Math.round(stats.cpu_temp_celsius) + ' °C';
        const load = (stats.load_avg || [0, 0, 0]).map(v => v.toFixed(2)).join(', ');
        return `<section class="card">
            <div class="net-card-head"><h2>${esc(stats.hostname || 'This device')}</h2><span class="tag tag-ok">Access point</span>${status && status.pending ? '<span class="tag tag-draft">Changes waiting</span>' : ''}</div>
            <dl class="inet-live">
                <div><dt>Up for</dt><dd>${uptimeText(stats.uptime_seconds)}</dd></div>
                <div><dt>Processor</dt><dd>${Math.round(stats.cpu_usage_pct || 0)}% busy, ${esc(temp)}</dd></div>
                <div><dt>Memory</dt><dd>${mem}% used</dd></div>
                <div><dt>Load</dt><dd>${esc(load)}</dd></div>
                <div><dt>Software</dt><dd>${esc(stats.peakroute_build || '–')}</dd></div>
            </dl>
        </section>`;
    }

    function networkCard() {
        const a = status.address;
        let state;
        if (!a.up) state = `${dot('bad')} The network bridge is down`;
        else if (!a.addresses.length) state = `${dot('warn')} ${a.mode === 'static' ? 'No address yet' : 'Waiting for an address from the other router'}`;
        else state = `${dot('ok')} On the network`;
        const ports = status.ports.map(p => {
            if (p.kind === 'link') {
                const conn = p.connected;
                const sig = p.signal_dbm;
                const text = conn == null ? `Wi-Fi link to ${esc(p.ssid)}` : conn ? `Connected to ${esc(p.ssid)}${sig != null ? `, ${signalWord(sig)} (${sig} dBm)` : ''}${p.rx_mbps ? `, ${Math.round(p.rx_mbps)} / ${Math.round(p.tx_mbps)} Mbit/s` : ''}` : `Not connected to ${esc(p.ssid)}`;
                return `<li>${dot(conn == null ? 'idle' : conn ? (sig != null ? signalClass(sig) : 'ok') : 'bad')}<strong>${esc(p.name)}</strong><span class="form-hint">${text}</span></li>`;
            }
            const up = p.state === 'up';
            return `<li>${dot(up ? 'ok' : p.state === 'missing' ? 'bad' : 'idle')}<strong>${esc(p.name)}</strong><span class="form-hint">${up ? 'Cable plugged in' + (p.speed && p.speed !== 'N/A' ? ', ' + esc(p.speed) : '') : p.state === 'missing' ? 'Not found in this device' : 'No cable'}</span></li>`;
        }).join('');
        return `<section class="card">
            <div class="net-card-head"><h2>On the network</h2><a href="#" class="view-all-link" data-goto="ap">Settings &rarr;</a></div>
            <p class="apx-state">${state}</p>
            <dl class="inet-live">
                <div><dt>Address</dt><dd>${a.addresses.length ? a.addresses.map(esc).join(', ') : '–'} <span class="form-hint">${a.mode === 'static' ? 'fixed' : 'from DHCP'}</span></dd></div>
                <div><dt>Gateway</dt><dd>${esc(a.gateway || '–')}</dd></div>
                <div><dt>DNS</dt><dd>${a.dns.length ? a.dns.map(esc).join(', ') : '–'}</dd></div>
                <div><dt>Setup address</dt><dd>${a.recovery ? '192.168.77.1 <span class="form-hint">(a computer set to 192.168.77.x reaches this device there)</span>' : '–'}</dd></div>
            </dl>
            <h3 class="form-section">Ports and links</h3>
            ${ports ? `<ul class="apx-list">${ports}</ul>` : '<p class="form-hint">No cable port or link is part of the network.</p>'}
        </section>`;
    }

    function radioRow(r) {
        if (!r.up) return `<tr><td><strong>${esc(r.name)}</strong></td><td>${esc(r.band)}</td><td colspan="5">${dot('bad')} Not running. Check the adapter on the Wireless page.</td></tr>`;
        const busy = r.busy_percent;
        const busyCls = busy == null ? 'idle' : busy >= 70 ? 'bad' : busy >= 40 ? 'warn' : 'ok';
        return `<tr><td>${dot('ok')}<strong>${esc(r.name)}</strong></td><td>${esc(r.band)}</td>
            <td>channel ${r.channel}${r.width ? ', ' + r.width + ' MHz' : ''}</td>
            <td>${r.tx_power_dbm == null ? '–' : Math.round(r.tx_power_dbm) + ' dBm'}</td>
            <td>${busy == null ? '–' : `${dot(busyCls)}${Math.round(busy)}% busy`}</td>
            <td>${r.noise_dbm == null ? '–' : r.noise_dbm + ' dBm'}</td>
            <td>${r.clients}${r.avg_signal_dbm != null ? ` <span class="form-hint">avg ${r.avg_signal_dbm} dBm</span>` : ''}</td></tr>`;
    }

    function wifiCard() {
        if (!status.radios.length && !status.networks.length) {
            return `<section class="card"><div class="net-card-head"><h2>Wi-Fi</h2><a href="#" class="view-all-link" data-goto="wireless">Set up &rarr;</a></div>
                <p class="form-hint">This access point does not host Wi-Fi yet. Set up an adapter and a Wi-Fi network on the Wireless page.</p></section>`;
        }
        const radios = status.radios.length ? `<div class="apx-scroll"><table class="apx-table"><thead><tr><th>Adapter</th><th>Band</th><th>Channel</th><th>Power</th><th>Channel use</th><th>Noise</th><th>Devices</th></tr></thead><tbody>${status.radios.map(radioRow).join('')}</tbody></table></div>
            <p class="form-hint">Channel use is how much of the time the channel is busy with anything on air, ours or other networks'. Above 70% Wi-Fi gets slow: try another channel.</p>` : '';
        const nets = status.networks.length ? `<h3 class="form-section">Wi-Fi networks</h3><ul class="apx-list">${status.networks.map(n =>
            `<li>${dot(n.clients ? 'ok' : 'idle')}<strong>${esc(n.ssid)}</strong><span class="form-hint">${esc(n.bands.join(' + '))}${n.vlan ? ', VLAN ' + n.vlan : ''}${n.hidden ? ', hidden' : ''}</span><span class="tag">${n.clients} ${n.clients === 1 ? 'device' : 'devices'}</span></li>`).join('')}</ul>` : '';
        return `<section class="card"><div class="net-card-head"><h2>Wi-Fi</h2><a href="#" class="view-all-link" data-goto="wireless">Settings &rarr;</a></div>${radios}${nets}</section>`;
    }

    function devicesCard() {
        const list = status.devices;
        let body;
        if (!status.devices_known) body = '<p class="form-hint">The Wi-Fi side cannot be read on this system, so connected devices cannot be listed.</p>';
        else if (!list.length) body = '<p class="form-hint">No device is connected by Wi-Fi right now.</p>';
        else {
            const rows = list.map(d => {
                const sig = d.signal_dbm;
                return `<tr><td><code class="code-mac">${esc(d.mac)}</code></td><td>${esc(d.ssid)}</td><td>${esc(d.band)}${d.channel ? ', ch ' + d.channel : ''}</td>
                    <td>${sig == null ? '–' : `${dot(signalClass(sig))}${sig} dBm <span class="form-hint">${signalWord(sig)}</span>`}</td>
                    <td>${d.rx_mbps ? Math.round(d.rx_mbps) : '–'} / ${d.tx_mbps ? Math.round(d.tx_mbps) : '–'} Mbit/s</td>
                    <td>${bytesText(d.rx_bytes + d.tx_bytes)}</td><td>${agoText(d.connected_seconds)}</td></tr>`;
            }).join('');
            body = `<div class="apx-scroll"><table class="apx-table"><thead><tr><th>Device</th><th>Wi-Fi network</th><th>Band</th><th>Signal</th><th>Speed (down / up)</th><th>Traffic</th><th>Connected</th></tr></thead><tbody>${rows}</tbody></table></div>`;
        }
        return `<section class="card"><div class="net-card-head"><h2>Connected devices</h2><span class="tag">${list.length}</span></div>${body}</section>`;
    }

    function render() {
        const body = $('ap-home-body');
        if (!body) return;
        if (!status) {
            body.innerHTML = notYet ? '<section class="card"><h2>Ready to apply</h2><p class="form-hint">This device has been set up as an access point, but the settings are only a draft: nothing has been applied yet. Review and apply them with the bar at the top of the page, and this page will show how the access point is doing.</p></section>'
                : failed ? '<div class="form-message" role="alert">Could not read the status of this access point.</div>' : '<p class="form-hint">Loading…</p>';
            return;
        }
        body.innerHTML = deviceCard() + networkCard() + wifiCard() + devicesCard();
    }

    async function refresh() {
        const [a, b] = await Promise.all([apiFetch('/api/v1/ap/status'), apiFetch('/api/v1/system/stats')]);
        failed = !a || !a.ok;
        notYet = !!a && a.status === 409;
        if (a && a.ok) status = await a.json();
        else status = null;
        if (b && b.ok) stats = await b.json();
        render();
    }

    function init() {
        status = null; stats = null; failed = false;
        render();
        refresh();
        timer = setInterval(refresh, 5000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
