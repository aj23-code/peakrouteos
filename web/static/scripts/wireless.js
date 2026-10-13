// web/static/scripts/wireless.js
// Wireless page: the Wi-Fi adapters found and what each can do, how each adapter is used
// as an access point, and the Wi-Fi networks (one SSID each, joined to a Local Network). Saving only
// writes a draft. With no adapter the page says so and greys out the rest.

window.WirelessModule = (function () {
    const BANDS = ['2.4 GHz', '5 GHz'];

    let timer = null;
    let report = null;        // GET /api/v1/wireless: what the hardware can do
    let wifi = null;          // GET /api/v1/wifi: the draft setup
    let localNets = [];       // GET /api/v1/networks: where a Wi-Fi network can be joined
    let lastReport = '';
    let radios = {};          // adapter name -> { form, errors, message }
    let stations = {};        // adapter name -> { form, errors, message, confirmRemove }
    let ssids = [];           // { key, isNew, server, form, errors, message, confirmDelete }
    let selected = null;
    let busy = false;
    let wired = false;
    let newCount = 0;

    const $ = (id) => document.getElementById(id);
    const editable = () => !wifi || wifi.editable !== false;

    // ---------------------------------------------------------------- shared helpers

    function showMessage(text, kind) {
        const box = $('wifi-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    async function errorOf(res, fallback) {
        if (!res) return { general: 'The router did not answer. Try again.', fields: [] };
        let err = {};
        try { err = await res.json(); } catch (e) { /* not JSON */ }
        return { general: err.error || fallback, fields: err.errors || [] };
    }

    const optionHtml = (value, label, current) =>
        `<option value="${escapeHtml(value)}"${String(value) === String(current) ? ' selected' : ''}>${escapeHtml(label)}</option>`;

    function afterSave() {
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    // ---------------------------------------------------------------- adapters: what they can do

    function channelChips(band) {
        const usable = band.channels.filter(c => !c.disabled && !c.no_ir);
        const blocked = band.channels.length - usable.length;
        const chips = usable.map(c =>
            `<span class="chip${c.dfs ? ' chip--tag' : ''}" title="${c.mhz} MHz${c.dfs ? ', shares spectrum with radar: an access point listens before it starts' : ''}">${c.number}${c.dfs ? ' <small>DFS</small>' : ''}</span>`).join('');
        return `<div class="chips">${chips || '<span class="form-hint">No channel is open for an access point right now.</span>'}</div>
            ${blocked ? `<p class="form-hint">${blocked} more ${blocked === 1 ? 'channel is' : 'channels are'} not available here under the current country setting.</p>` : ''}`;
    }

    function bandHtml(band) {
        return `<div class="wifi-band">
            <div class="wifi-band-head"><strong>${escapeHtml(band.name)}</strong>
                <span class="form-hint">widths: ${band.widths.map(w => w + ' MHz').join(', ')}</span></div>
            ${channelChips(band)}
        </div>`;
    }

    // ---------------------------------------------------------------- adapters: use as an access point

    const bandOf = (a, name) => a.bands.find(b => b.name === name);

    // 20 MHz on 2.4 GHz (wider channels crowd it), the widest up to 80 MHz on 5 GHz
    function defaultWidth(band) {
        const widths = band.widths.slice().sort((x, y) => x - y);
        if (band.name === '2.4 GHz' && widths.includes(20)) return 20;
        const fit = widths.filter(w => w <= 80);
        return (fit.length ? fit : widths).slice(-1)[0] || 20;
    }

    function radioFormOf(a) {
        const r = ((wifi && wifi.radios) || []).find(x => x.name === a.name);
        if (r) return { enabled: r.enabled, band: r.band, channel: String(r.channel), width: String(r.width) };
        const band = bandOf(a, '5 GHz') || a.bands[0];
        return { enabled: false, band: band ? band.name : '', channel: '0', width: band ? String(defaultWidth(band)) : '20' };
    }

    const radioNorm = (f) => JSON.stringify([f.enabled, f.band, f.channel, f.width]);

    function radioDirty(a) {
        const st = radios[a.name];
        const server = ((wifi && wifi.radios) || []).find(x => x.name === a.name);
        if (!server && !st.form.enabled) return false;
        return radioNorm(st.form) !== radioNorm(radioFormOf(a));
    }

    function radioState(a) {
        if (!radios[a.name]) radios[a.name] = { form: radioFormOf(a), errors: {}, message: null };
        return radios[a.name];
    }

    function channelOptions(band, current) {
        let html = optionHtml('0', 'Automatic', current);
        if (band) {
            band.channels.filter(c => !c.disabled).forEach(c => {
                const notes = [];
                if (c.dfs) notes.push('DFS');
                if (c.no_ir) notes.push('closed under the country in effect');
                html += optionHtml(String(c.number), c.number + (notes.length ? ' (' + notes.join(', ') + ')' : ''), current);
            });
        }
        return html;
    }

    const serverStation = (a) => ((wifi && wifi.stations) || []).find(x => x.name === a.name);
    const serverRadio = (a) => ((wifi && wifi.radios) || []).find(x => x.name === a.name);

    function radioHtml(a) {
        if (!a.capabilities_known || !a.ap_capable) return '';
        const stn = serverStation(a);
        if (stn && stn.enabled) {
            return `<div class="wifi-radio"><div class="net-card-head"><h3 class="form-section">${escapeHtml(a.name)}</h3>${adapterBadges(a)}</div><p class="form-hint">This adapter is used as a Wi-Fi client (see Point-to-point links and wireless uplink). An adapter either hosts Wi-Fi or joins it: switch the client off and save to host Wi-Fi with it.</p></div>`;
        }
        const st = radioState(a);
        const f = st.form;
        const band = bandOf(a, f.band);
        const server = ((wifi && wifi.radios) || []).find(x => x.name === a.name);
        const lock = !editable() || busy;
        const dirty = radioDirty(a);
        const off = lock || !f.enabled;
        const err = (field) => st.errors[field] ? `<p class="field-error">${escapeHtml(st.errors[field])}</p>` : '';
        const key = escapeHtml(a.name);

        return `<div class="wifi-radio" data-adapter="${key}">
            <div class="net-card-head"><h3 class="form-section">${key}</h3>${adapterBadges(a)}${server && server.pending ? '<span class="tag tag-draft">Draft</span>' : ''}</div>
            <label class="toggle toggle--block"><input type="checkbox" data-adapter="${key}" data-radio="enabled"${f.enabled ? ' checked' : ''}${lock ? ' disabled' : ''}>
                <span><strong>Host Wi-Fi networks with this adapter</strong><span class="form-hint">Devices join the Wi-Fi networks above through it, on the band, channel and width chosen here.</span></span></label>
            <div class="form-grid${f.enabled ? '' : ' is-off'}">
                <div class="form-field"><label for="radio-${key}-band">Band</label>
                    <select id="radio-${key}-band" class="role-select" data-adapter="${key}" data-radio="band"${off ? ' disabled' : ''}>
                        ${a.bands.map(b => optionHtml(b.name, b.name, f.band)).join('')}</select>${err('band')}</div>
                <div class="form-field"><label for="radio-${key}-channel">Channel</label>
                    <select id="radio-${key}-channel" class="role-select" data-adapter="${key}" data-radio="channel"${off ? ' disabled' : ''}>
                        ${channelOptions(band, f.channel)}</select>${err('channel')}</div>
                <div class="form-field"><label for="radio-${key}-width">Channel width</label>
                    <select id="radio-${key}-width" class="role-select" data-adapter="${key}" data-radio="width"${off ? ' disabled' : ''}>
                        ${(band ? band.widths : [20]).map(w => optionHtml(String(w), w + ' MHz', f.width)).join('')}</select>${err('width')}</div>
            </div>
            <p class="form-hint">Automatic picks the quietest channel when Wi-Fi starts. Wider channels are faster but crowd the 2.4 GHz band.</p>
            ${st.message ? `<div class="form-message${st.message.kind === 'ok' ? ' form-message--ok' : ''}" role="alert">${escapeHtml(st.message.text)}</div>` : ''}
            <div class="form-actions">
                <span class="form-hint">${dirty ? 'You have unsaved changes.' : ''}</span>
                <button type="button" class="cfg-btn cfg-btn--ghost radio-reset" data-adapter="${key}"${busy || !dirty ? ' disabled' : ''}>Reset</button>
                <button type="button" class="cfg-btn cfg-btn--primary radio-save" data-adapter="${key}"${lock || !dirty ? ' disabled' : ''}>Save to draft</button>
            </div>
        </div>`;
    }

    // ---------------------------------------------------------------- adapters: use as a Wi-Fi client

    const ROLES = [
        ['uplink', 'The internet connection', 'Wireless uplink: this adapter becomes the internet port. The router joins another Wi-Fi network (a hotel, a café, a neighbour) and shares it. A cable port that was the internet port is left unassigned.'],
        ['bridge', 'A link into a local network', 'The far end of a point-to-point link, or a wireless extender: the adapter joins a local network like a cable port, over the air, and devices behind it keep their own addresses. The other end must accept point-to-point links.'],
    ];

    function stationFormOf(a) {
        const s0 = serverStation(a);
        if (s0) return { enabled: s0.enabled, ssid: s0.ssid || '', passphrase: s0.passphrase || '', security: s0.security || 'wpa2-wpa3', hidden: !!s0.hidden,
            role: s0.role || 'uplink', network: s0.network || 'lan', show: false };
        return { enabled: false, ssid: '', passphrase: '', security: 'wpa2-wpa3', hidden: false, role: 'uplink', network: 'lan', show: false };
    }

    const stationNorm = (f) => JSON.stringify([f.enabled, f.ssid, f.passphrase, f.security, f.hidden, f.role, f.role === 'bridge' ? f.network : '']);

    function stationState(a) {
        if (!stations[a.name]) stations[a.name] = { form: stationFormOf(a), errors: {}, message: null, confirmRemove: false };
        return stations[a.name];
    }

    function stationDirty(a) {
        const st = stationState(a);
        if (!serverStation(a) && !st.form.enabled) return false;
        return stationNorm(st.form) !== stationNorm(stationFormOf(a));
    }

    function stationHtml(a) {
        const canClient = !a.capabilities_known || a.modes.includes('managed');
        if (!canClient) return '';
        const rad = serverRadio(a);
        if (rad && rad.enabled) {
            return `<div class="wifi-station"><div class="net-card-head"><h3 class="form-section">${escapeHtml(a.name)}</h3>${adapterBadges(a)}</div><p class="form-hint">This adapter hosts Wi-Fi networks (see Wi-Fi networks above). An adapter either hosts Wi-Fi or joins it: switch the access point off and save to use it as a client.</p></div>`;
        }
        const st = stationState(a);
        const f = st.form;
        const server = serverStation(a);
        const lock = !editable() || busy;
        const off = lock || !f.enabled;
        const dirty = stationDirty(a);
        const key = escapeHtml(a.name);
        const err = (field) => st.errors[field] ? `<p class="field-error">${escapeHtml(st.errors[field])}</p>` : '';
        const netOptions = (localNets.length ? localNets : [{ id: 'lan', default: true }])
            .map(n => optionHtml(n.id, n.default ? 'LAN' : `${n.name} (VLAN ${n.vlan})`, f.network)).join('');
        const notUsed = server && server.enabled && !server.in_use && server.role === 'uplink';
        const SECNAME = { wpa2: 'WPA2', wpa3: 'WPA3', 'wpa2-wpa3': 'WPA2/WPA3', open: 'Open', wep: 'WEP', enterprise: 'Enterprise' };
        const scan = st.scan;
        const scanHtml = !f.enabled ? '' : `<div class="wifi-scan">
            <button type="button" class="cfg-btn st-scan" data-st="${key}"${lock || (scan && scan.busy) ? ' disabled' : ''}>${scan && scan.busy ? 'Scanning…' : 'Scan for networks'}</button>
            ${scan && scan.error ? `<p class="field-error">${escapeHtml(scan.error)}</p>` : ''}
            ${scan && scan.networks ? (scan.cached ? '<p class="form-hint">A new scan is not allowed here: this is what the adapter remembers from an earlier one.</p>' : '') +
                (scan.networks.length ? `<ul class="scan-list">${scan.networks.map((n, i) => `<li><button type="button" class="scan-pick st-pick${n.supported ? '' : ' is-unsupported'}" data-st="${key}" data-pick="${i}"${n.supported ? '' : ' disabled'}>
                    <strong>${escapeHtml(n.ssid)}</strong><span class="form-hint">${escapeHtml(n.band)}, ${n.signal_dbm} dBm</span><span class="tag${n.supported ? '' : ' tag-warn'}">${escapeHtml(SECNAME[n.security] || n.security)}${n.connected ? ', connected' : ''}${n.supported ? '' : ' (cannot be joined)'}</span></button></li>`).join('')}</ul>` : '<p class="form-hint">No network was found.</p>') : ''}
        </div>`;
        const input = (field, label, type, extra) => `<div class="form-field"><label for="st-${key}-${field}">${label}</label>
            ${field === 'passphrase'
                ? `<div class="pass-row"><input type="${f.show ? 'text' : 'password'}" id="st-${key}-${field}" data-st="${key}" data-stf="${field}" value="${escapeHtml(f[field])}" autocomplete="off" spellcheck="false" autocapitalize="off"${st.errors[field] ? ' aria-invalid="true"' : ''}${off ? ' disabled' : ''}><button type="button" class="cfg-btn cfg-btn--ghost st-reveal" data-st="${key}" aria-pressed="${f.show}">${f.show ? 'Hide' : 'Show'}</button></div>`
                : `<input type="${type}" id="st-${key}-${field}" data-st="${key}" data-stf="${field}" value="${escapeHtml(f[field])}" autocomplete="off" spellcheck="false" autocapitalize="off" ${extra || ''}${st.errors[field] ? ' aria-invalid="true"' : ''}${off ? ' disabled' : ''}>`}
            ${err(field)}</div>`;

        return `<div class="wifi-station" data-adapter="${key}">
            <div class="net-card-head"><h3 class="form-section">${key}</h3>${adapterBadges(a)}${server && server.pending ? '<span class="tag tag-draft">Draft</span>' : ''}</div>
            <label class="toggle toggle--block"><input type="checkbox" data-st="${key}" data-stf="enabled"${f.enabled ? ' checked' : ''}${lock ? ' disabled' : ''}>
                <span><strong>Use this adapter as a Wi-Fi client</strong><span class="form-hint">Instead of hosting a network, the adapter joins another one, the way a phone does: the other end of a link, or someone else's Wi-Fi.</span></span></label>
            ${scanHtml}
            <div class="form-grid${f.enabled ? '' : ' is-off'}">
                ${input('ssid', 'Network name', 'text', 'placeholder="Name of the Wi-Fi network"')}
                ${input('passphrase', 'Password')}
                <div class="form-field"><label for="st-${key}-security">Security</label>
                    <select id="st-${key}-security" class="role-select" data-st="${key}" data-stf="security"${off ? ' disabled' : ''}>${SECURITY.map(x => optionHtml(x[0], x[1], f.security)).join('')}</select>${err('security')}</div>
                <div class="form-field"><label class="toggle"><input type="checkbox" data-st="${key}" data-stf="hidden"${f.hidden ? ' checked' : ''}${off ? ' disabled' : ''}> <span>The network's name is hidden</span></label></div>
            </div>
            <h3 class="form-section">Use it as</h3>
            <div class="net-access">
                ${ROLES.map(r => `<label class="toggle toggle--block"><input type="radio" name="st-role-${key}" data-st="${key}" data-stf="role" value="${r[0]}"${f.role === r[0] ? ' checked' : ''}${off ? ' disabled' : ''}>
                    <span><strong>${r[1]}</strong><span class="form-hint">${r[2]}</span></span></label>`).join('')}
            </div>
            ${err('role')}
            ${f.role === 'bridge' ? `<div class="form-grid"><div class="form-field"><label for="st-${key}-network">Local Network</label>
                <select id="st-${key}-network" class="role-select" data-st="${key}" data-stf="network"${off ? ' disabled' : ''}>${netOptions}</select>${err('network')}</div></div>` : ''}
            ${notUsed ? `<p class="form-hint net-warn">Not in use: the internet port is <strong>${escapeHtml(wifi.wan_port || '?')}</strong> now. Save again to make this adapter the internet port.</p>` : ''}
            ${(!wifi.country && f.enabled) ? '<p class="form-hint net-warn">A country must be chosen in Settings before this can be committed.</p>' : ''}
            ${st.message ? `<div class="form-message${st.message.kind === 'ok' ? ' form-message--ok' : ''}" role="alert">${escapeHtml(st.message.text)}</div>` : ''}
            <div class="form-actions">
                ${server ? `<button type="button" class="cfg-btn cfg-btn--danger st-remove" data-st="${key}"${lock ? ' disabled' : ''}>${st.confirmRemove ? 'Click again to remove' : 'Stop using as a client'}</button>` : ''}
                <span class="form-hint">${dirty ? 'You have unsaved changes.' : ''}</span>
                <button type="button" class="cfg-btn cfg-btn--ghost st-reset" data-st="${key}"${busy || !dirty ? ' disabled' : ''}>Reset</button>
                <button type="button" class="cfg-btn cfg-btn--primary st-save" data-st="${key}"${lock || !dirty ? ' disabled' : ''}>Save to draft</button>
            </div>
        </div>`;
    }

    const ST_FIELDS = ['ssid', 'passphrase', 'security', 'role', 'network', 'name'];

    async function scanFor(a) {
        const st = stationState(a);
        st.scan = { busy: true, networks: null, error: null, cached: false };
        renderAdapters();
        const res = await apiFetch('/api/v1/wifi/scan?adapter=' + encodeURIComponent(a.name));
        if (!res || !res.ok) {
            const e = await errorOf(res, 'The scan failed.');
            st.scan = { busy: false, networks: null, error: e.general, cached: false };
        } else {
            const d = await res.json();
            st.scan = { busy: false, networks: d.networks, error: null, cached: d.cached };
        }
        renderAdapters();
    }

    function pickNetwork(a, i) {
        const st = stationState(a);
        const n = st.scan && st.scan.networks && st.scan.networks[i];
        if (!n || !n.supported) return;
        st.form.ssid = n.ssid;
        st.form.security = n.security;
        st.form.hidden = false;
        delete st.errors.ssid; delete st.errors.security;
        st.message = null;
        renderAdapters();
        const f = document.getElementById(`st-${a.name}-passphrase`);
        if (f) f.focus();
    }

    async function saveStation(a) {
        const st = stationState(a);
        const f = st.form;
        st.message = null;
        st.errors = {};
        busy = true;
        renderAdapters();
        const res = await apiFetch('/api/v1/wifi/stations/' + encodeURIComponent(a.name), {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ ssid: f.ssid, passphrase: f.passphrase, security: f.security, hidden: f.hidden, role: f.role, network: f.role === 'bridge' ? f.network : '', enabled: f.enabled }),
        });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, `Saving failed${res ? ' (' + res.status + ')' : ''}.`);
            const general = [];
            e.fields.forEach(x => { if (ST_FIELDS.includes(x.field)) st.errors[x.field] = x.message; else general.push(x.message); });
            st.message = { text: general.join(' ') || (Object.keys(st.errors).length ? 'Some settings need attention.' : e.general), kind: 'error' };
            renderAdapters();
            return;
        }
        await load();
        stations[a.name] = { form: stationFormOf(a), errors: {}, message: { text: 'Saved as a draft. Review and commit it from the bar at the top when you are ready.', kind: 'ok' }, confirmRemove: false };
        renderAdapters();
        afterSave();
    }

    async function removeStation(a) {
        const st = stationState(a);
        if (!st.confirmRemove) {
            st.confirmRemove = true;
            renderAdapters();
            setTimeout(() => { if (st.confirmRemove) { st.confirmRemove = false; renderAdapters(); } }, 4000);
            return;
        }
        st.confirmRemove = false;
        busy = true;
        renderAdapters();
        const res = await apiFetch('/api/v1/wifi/stations/' + encodeURIComponent(a.name), { method: 'DELETE' });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, 'The Wi-Fi client could not be removed.');
            st.message = { text: e.fields.map(x => x.message).join(' ') || e.general, kind: 'error' };
            renderAdapters();
            return;
        }
        delete stations[a.name];
        showMessage('Wi-Fi client removed from the draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        await load();
        afterSave();
    }

    function adapterBadges(a) {
        let badge;
        if (!a.capabilities_known) badge = '<span class="tag">Capabilities unknown</span>';
        else if (a.ap_capable) badge = '<span class="tag tag-ok">Can host Wi-Fi</span>';
        else badge = '<span class="tag tag-warn">Cannot host Wi-Fi</span>';
        const srv = serverStation(a);
        if (srv && srv.enabled) badge += ` <span class="tag tag-ok">Wi-Fi client${srv.in_use ? '' : ' (not in use)'}</span>`;
        return badge + ` <span class="tag">${escapeHtml(a.state || 'unknown')}</span>`;
    }

    // the facts of one adapter (the Adapters section)
    function adapterHtml(a) {
        return `<section class="wifi-adapter" data-adapter="${escapeHtml(a.name)}">
            <div class="net-card-head"><h3 class="form-section">${escapeHtml(a.name)}</h3>${adapterBadges(a)}</div>
            <dl class="inet-live">
                <div><dt>MAC address</dt><dd><code class="code-mac">${escapeHtml(a.mac || '-')}</code></dd></div>
                <div><dt>Driver</dt><dd>${a.driver ? `<span class="badge-driver">${escapeHtml(a.driver)}</span>` : '-'}</dd></div>
                <div><dt>Radio</dt><dd>${escapeHtml(a.phy || '-')}</dd></div>
                <div><dt>Modes</dt><dd>${a.modes.length ? escapeHtml(a.modes.join(', ')) : '-'}</dd></div>
                ${a.max_ap ? `<div><dt>Wi-Fi networks at once</dt><dd>${a.max_ap}</dd></div>` : ''}
            </dl>
            <details class="wifi-caps"><summary>Bands and channels</summary>${a.bands.map(bandHtml).join('')}</details>
        </section>`;
    }

    const apBlock = (a) => a.capabilities_known && a.ap_capable ? radioHtml(a)
        : `<div class="wifi-radio"><div class="net-card-head"><h3 class="form-section">${escapeHtml(a.name)}</h3>${adapterBadges(a)}</div><p class="form-hint">${a.capabilities_known ? 'This adapter cannot host Wi-Fi networks.' : 'What this adapter can do could not be read, so it cannot be set up to host Wi-Fi.'}</p></div>`;

    function renderAdapters() {
        const list = report ? report.adapters : [];
        $('wifi-aps').innerHTML = list.map(apBlock).join('');
        $('wifi-links').innerHTML = list.map(stationHtmlOrNote).join('');
        $('wifi-adapters').innerHTML = list.map(adapterHtml).join('');
        sectionSummaries();
    }

    const stationHtmlOrNote = (a) => stationHtml(a) || `<div class="wifi-station"><div class="net-card-head"><h3 class="form-section">${escapeHtml(a.name)}</h3>${adapterBadges(a)}</div><p class="form-hint">This adapter cannot join Wi-Fi networks as a client.</p></div>`;

    // one line on each closed section, so that it says what is set up without opening it
    function sectionSummaries() {
        if (!wifi || !report) return;
        const nets = (wifi.networks || []).filter(n => n.enabled).length;
        const aps = (wifi.radios || []).filter(r => r.enabled).length;
        const stn = (wifi.stations || []).filter(x => x.enabled);
        const wds = (wifi.networks || []).filter(n => n.wds && n.enabled).length;
        const plural = (n, one, many) => `${n} ${n === 1 ? one : many}`;
        $('wifi-sec-ap-sum').textContent = nets || aps ? `${plural(nets, 'Wi-Fi network', 'Wi-Fi networks')} on, ${plural(aps, 'adapter', 'adapters')} hosting` : 'Nothing set up';
        const parts = stn.map(x => `${x.name}: ${x.role === 'uplink' ? 'internet connection' : 'link into a local network'}`);
        if (wds) parts.push(`accepting links on ${plural(wds, 'network', 'networks')}`);
        $('wifi-sec-link-sum').textContent = parts.length ? parts.join('; ') : 'Not used';
        $('wifi-sec-adapters-sum').textContent = plural(report.adapters.length, 'adapter', 'adapters');
    }

    function notes(r) {
        const out = [];
        // the country is a router-wide setting (System, Settings); Wi-Fi only uses it
        if (wifi && wifi.country) {
            out.push(`Country: <strong>${escapeHtml(wifi.country)}</strong>. Change it in <a href="#" data-goto="settings">Settings</a>.`);
        } else {
            out.push('<strong>No country is set.</strong> Wi-Fi cannot be switched on until you choose one in <a href="#" data-goto="settings">Settings</a>: it decides which channels may be used.');
        }
        if (!r.iw_available) {
            out.push('The tool that reads what an adapter can do (<code>iw</code>) is not available on this system, so bands and channels cannot be shown.');
        } else if (r.country_known) {
            const world = r.country === '00';
            out.push(`The regulatory country in effect now: <strong>${escapeHtml(r.country)}</strong>${world ? ' (the world setting: the strictest, so most 5 GHz channels are closed until Wi-Fi starts with your country).' : ''}`);
        }
        return out.map(n => `<p class="form-hint wifi-note">${n}</p>`).join('');
    }

    async function saveRadio(a) {
        const st = radioState(a);
        const f = st.form;
        st.message = null;
        st.errors = {};
        busy = true;
        renderAdapters();
        const res = await apiFetch('/api/v1/wifi/radios/' + encodeURIComponent(a.name), {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ band: f.band, channel: parseInt(f.channel, 10) || 0, width: parseInt(f.width, 10) || 20, enabled: f.enabled }),
        });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, `Saving failed${res ? ' (' + res.status + ')' : ''}.`);
            const general = [];
            e.fields.forEach(x => { if (['band', 'channel', 'width'].includes(x.field)) st.errors[x.field] = x.message; else general.push(x.message); });
            st.message = { text: general.join(' ') || (Object.keys(st.errors).length ? 'Some settings need attention.' : e.general), kind: 'error' };
            renderAdapters();
            return;
        }
        wifi = await res.json();
        radios[a.name] = { form: radioFormOf(a), errors: {}, message: { text: 'Saved as a draft. Review and commit it from the bar at the top when you are ready.', kind: 'ok' } };
        renderAll();
        afterSave();
    }

    // ---------------------------------------------------------------- Wi-Fi networks

    const SECURITY = [
        ['wpa2-wpa3', 'WPA2 + WPA3 (recommended)'],
        ['wpa3', 'WPA3 only'],
        ['wpa2', 'WPA2 only'],
    ];

    function ssidFormOf(n) {
        return {
            ssid: n.ssid || '', passphrase: n.passphrase || '', network: n.network || 'lan', security: n.security || 'wpa2-wpa3',
            b24: (n.bands || []).includes('2.4 GHz'), b5: (n.bands || []).includes('5 GHz'),
            steering: !!n.band_steering, hidden: !!n.hidden, isolation: !!n.client_isolation, wds: !!n.wds, enabled: n.enabled !== false,
            show: false,
        };
    }

    const ssidNorm = (f) => JSON.stringify([f.ssid, f.passphrase, f.network, f.security, f.b24, f.b5, f.steering, f.hidden, f.isolation, f.wds, f.enabled]);
    const ssidDirty = (c) => c.isNew || ssidNorm(c.form) !== ssidNorm(ssidFormOf(c.server));

    function bandsOf(f) { return BANDS.filter(b => (b === '2.4 GHz' ? f.b24 : f.b5)); }

    // the bands an access point is switched on for in the draft, else the ones adapters could serve
    function offeredBands() {
        const on = new Set(((wifi && wifi.radios) || []).filter(r => r.enabled).map(r => r.band));
        if (on.size) return on;
        const any = new Set();
        ((report && report.adapters) || []).forEach(a => { if (a.ap_capable) a.bands.forEach(b => any.add(b.name)); });
        return any;
    }

    function ssidLabel(c) {
        if (c.isNew) return (c.form.ssid.trim() || 'New Wi-Fi network') + ' (new)';
        let label = c.server.ssid || c.server.id;
        if (ssidDirty(c)) label += ' • unsaved';
        else if (c.server.pending) label += ' • draft';
        return label;
    }

    function networkName(id) {
        const n = localNets.find(x => x.id === id);
        return n ? (n.default ? 'LAN' : `${n.name} (VLAN ${n.vlan})`) : id;
    }

    function ssidInput(c, field, label, opts) {
        const id = `${c.key}-${field}`;
        const err = c.errors[field];
        const off = !editable() || busy;
        const type = field === 'passphrase' && !c.form.show ? 'password' : 'text';
        const input = `<input type="${type}" id="${id}" data-key="${c.key}" data-field="${field}" value="${escapeHtml(c.form[field])}"
            ${opts.placeholder ? `placeholder="${escapeHtml(opts.placeholder)}"` : ''} autocomplete="off" spellcheck="false" autocapitalize="off"
            ${err ? 'aria-invalid="true"' : ''}${off ? ' disabled' : ''}>`;
        const body = field === 'passphrase'
            ? `<div class="pass-row">${input}<button type="button" class="cfg-btn cfg-btn--ghost ssid-reveal" data-key="${c.key}" aria-pressed="${c.form.show}">${c.form.show ? 'Hide' : 'Show'}</button></div>`
            : input;
        return `<div class="form-field${opts.wide ? ' form-field--wide' : ''}"><label for="${id}">${label}</label>${body}
            ${err ? `<p class="field-error">${escapeHtml(err)}</p>` : ''}</div>`;
    }

    function ssidSelect(c, field, label, options) {
        const id = `${c.key}-${field}`;
        const err = c.errors[field];
        return `<div class="form-field"><label for="${id}">${label}</label>
            <select id="${id}" class="role-select" data-key="${c.key}" data-field="${field}"${!editable() || busy ? ' disabled' : ''}${err ? ' aria-invalid="true"' : ''}>${options}</select>
            ${err ? `<p class="field-error">${escapeHtml(err)}</p>` : ''}</div>`;
    }

    function ssidToggle(c, field, label, hint) {
        return `<label class="toggle toggle--block"><input type="checkbox" data-key="${c.key}" data-field="${field}"${c.form[field] ? ' checked' : ''}${!editable() || busy ? ' disabled' : ''}>
            <span><strong>${label}</strong><span class="form-hint">${hint}</span></span></label>`;
    }

    function ssidCardHtml(c) {
        const f = c.form;
        const lock = !editable() || busy;
        const dirty = ssidDirty(c);
        const both = f.b24 && f.b5;
        const missing = bandsOf(f).filter(b => !(wifi.radios || []).some(r => r.enabled && r.band === b));
        const netOptions = (localNets.length ? localNets : [{ id: 'lan', default: true }])
            .map(n => optionHtml(n.id, n.default ? 'LAN' : `${n.name} (VLAN ${n.vlan})`, f.network)).join('');
        const bandErr = c.errors.bands ? `<p class="field-error">${escapeHtml(c.errors.bands)}</p>` : '';

        return `<section class="card net-card" data-key="${c.key}">
            <div class="net-card-head">
                <h2>${escapeHtml(c.isNew ? (f.ssid.trim() || 'New Wi-Fi network') : (c.server.ssid || c.server.id))}</h2>
                ${!c.isNew && !c.server.enabled ? '<span class="tag">Off</span>' : ''}
                ${!c.isNew && c.server.pending ? '<span class="tag tag-draft">Draft</span>' : ''}
                ${c.isNew ? '<span class="tag tag-draft">New</span>' : ''}
            </div>

            <div class="form-grid">
                ${ssidInput(c, 'ssid', 'Wi-Fi name', { placeholder: 'Guest Wi-Fi' })}
                ${ssidInput(c, 'passphrase', 'Password', { placeholder: 'At least 8 characters' })}
                ${ssidSelect(c, 'security', 'Security', SECURITY.map(s => optionHtml(s[0], s[1], f.security)).join(''))}
                ${ssidSelect(c, 'network', 'Local Network', netOptions)}
            </div>
            <p class="form-hint">Devices that join this Wi-Fi network end up on <strong>${escapeHtml(networkName(f.network))}</strong>, with its addresses and rules.</p>

            <h3 class="form-section">Bands</h3>
            <div class="net-access">
                ${BANDS.map(b => `<label class="toggle toggle--block"><input type="checkbox" data-key="${c.key}" data-field="${b === '2.4 GHz' ? 'b24' : 'b5'}"${(b === '2.4 GHz' ? f.b24 : f.b5) ? ' checked' : ''}${lock ? ' disabled' : ''}>
                    <span><strong>${b}</strong><span class="form-hint">${b === '2.4 GHz' ? 'Longer range, slower, crowded.' : 'Faster, shorter range.'}</span></span></label>`).join('')}
            </div>
            ${bandErr}
            ${missing.length ? `<p class="form-hint net-warn">No adapter is switched on for ${missing.map(escapeHtml).join(' and ')} yet: set one up below, or this cannot be committed.</p>` : ''}
            ${both ? ssidToggle(c, 'steering', 'Band steering', 'Nudges devices that can use 5 GHz onto it, which keeps the slower 2.4 GHz band free. Devices that only have 2.4 GHz are not affected.') : ''}

            <h3 class="form-section">Options</h3>
            <div class="net-access">
                ${ssidToggle(c, 'enabled', 'Switched on', 'Off, the network is kept here but not broadcast.')}
                ${ssidToggle(c, 'hidden', 'Hide the name', 'The network does not show up in the list of Wi-Fi networks; devices have to be told its name.')}
                ${ssidToggle(c, 'isolation', 'Isolate devices from each other', 'Devices on this Wi-Fi network cannot talk to each other, only to the network behind it.')}
            </div>
            <details class="wifi-ptp"${f.wds ? ' open' : ''}>
                <summary>Point-to-point link (for linking two sites)</summary>
                <p class="form-hint">Only for linking this device to another one over the air, the way outdoor point-to-point radios do. Turn this on at the <em>near</em> end; at the far end set an adapter as a Wi-Fi client with <em>A link into a local network</em> (under Point-to-point links and wireless uplink). Use a name and password that nobody else knows.</p>
                <div class="net-access">${ssidToggle(c, 'wds', 'Accept point-to-point links', 'The far end joins this Wi-Fi network in 4-address mode, so the two sites become one network.')}</div>
            </details>

            ${c.message ? `<div class="form-message${c.message.kind === 'ok' ? ' form-message--ok' : ''}" role="alert">${escapeHtml(c.message.text)}</div>` : ''}

            <div class="form-actions">
                ${c.isNew ? '' : `<button type="button" class="cfg-btn cfg-btn--danger ssid-delete" data-key="${c.key}"${lock ? ' disabled' : ''}>${c.confirmDelete ? 'Click again to delete' : 'Delete Wi-Fi network'}</button>`}
                <span class="form-hint ssid-state" data-key="${c.key}">${!editable() ? 'A change is being applied or waits for confirmation: Wi-Fi cannot be edited right now.' : (dirty && !c.isNew ? 'You have unsaved changes.' : '')}</span>
                ${c.isNew
                    ? `<button type="button" class="cfg-btn cfg-btn--ghost ssid-cancel" data-key="${c.key}"${busy ? ' disabled' : ''}>Cancel</button>`
                    : `<button type="button" class="cfg-btn cfg-btn--ghost ssid-reset" data-key="${c.key}"${busy || !dirty ? ' disabled' : ''}>Reset</button>`}
                <button type="button" class="cfg-btn cfg-btn--primary ssid-save" data-key="${c.key}"${lock || !dirty ? ' disabled' : ''}>${c.isNew ? 'Add Wi-Fi network' : 'Save to draft'}</button>
            </div>
        </section>`;
    }

    function renderSsids() {
        const holder = $('wifi-ssids');
        if (!wifi || !report) { holder.innerHTML = ''; return; }
        const capable = report.adapters.some(a => a.ap_capable);
        if (!cardOf(selected)) selected = ssids.length ? ssids[0].key : null;
        const c = cardOf(selected);
        const lock = !editable() || busy;

        holder.innerHTML = `<section class="card wifi-ssids${capable ? '' : ' is-disabled'}" id="wifi-ssids-card">
            <div class="net-card-head">
                <h2>Wi-Fi networks</h2>
                <div class="net-toolbar wifi-toolbar">
                    ${ssids.length ? `<label class="net-picker"><span class="form-hint">Wi-Fi network</span>
                        <select id="wifi-select" class="role-select" aria-label="Choose a Wi-Fi network">${ssids.map(x => optionHtml(x.key, ssidLabel(x), selected)).join('')}</select></label>` : ''}
                    <button type="button" id="wifi-add" class="cfg-btn"${lock || ssids.some(x => x.isNew) || !capable ? ' disabled' : ''}>Add Wi-Fi network</button>
                </div>
            </div>
            ${capable ? '' : '<p class="form-hint">None of the adapters in this machine can act as an access point, so there is nothing to host a Wi-Fi network.</p>'}
            ${c ? '' : '<p class="form-hint">No Wi-Fi network yet. Add one to give a Local Network a Wi-Fi name and password.</p>'}
        </section>${c ? ssidCardHtml(c) : ''}`;
    }

    function cardOf(key) { return ssids.find(c => c.key === key); }

    function ssidBody(c) {
        const f = c.form;
        const bands = bandsOf(f);
        return {
            ssid: f.ssid, passphrase: f.passphrase, network: f.network, security: f.security, bands,
            band_steering: bands.length === 2 && f.steering, hidden: f.hidden, client_isolation: f.isolation, wds: f.wds, enabled: f.enabled,
        };
    }

    const SSID_FIELD = { ssid: 'ssid', passphrase: 'passphrase', network: 'network', security: 'security', bands: 'bands' };

    async function saveSsid(c) {
        c.message = null;
        c.errors = {};
        busy = true;
        renderSsids();
        const url = c.isNew ? '/api/v1/wifi/networks' : '/api/v1/wifi/networks/' + encodeURIComponent(c.server.id);
        const res = await apiFetch(url, { method: c.isNew ? 'POST' : 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(ssidBody(c)) });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, `Saving failed${res ? ' (' + res.status + ')' : ''}.`);
            const general = [];
            e.fields.forEach(x => {
                const f = SSID_FIELD[x.field];
                if (f) c.errors[f] = c.errors[f] ? c.errors[f] + ' ' + x.message : x.message; else general.push(x.message);
            });
            c.message = { text: general.join(' ') || (Object.keys(c.errors).length ? 'Some settings need attention.' : e.general), kind: 'error' };
            renderSsids();
            return;
        }
        const saved = await res.json();
        if (c.isNew) selected = saved.id;
        ssids = ssids.filter(x => x !== c);
        c.justSaved = true;
        showMessage(c.isNew ? 'Wi-Fi network added to the draft. Review and commit it from the bar at the top when you are ready.'
            : 'Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        await load();
        afterSave();
    }

    async function removeSsid(c) {
        if (!c.confirmDelete) {
            c.confirmDelete = true;
            renderSsids();
            setTimeout(() => { if (c.confirmDelete) { c.confirmDelete = false; renderSsids(); } }, 4000);
            return;
        }
        c.confirmDelete = false;
        busy = true;
        renderSsids();
        const res = await apiFetch('/api/v1/wifi/networks/' + encodeURIComponent(c.server.id), { method: 'DELETE' });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, 'The Wi-Fi network could not be deleted.');
            c.message = { text: e.fields.map(x => x.message).join(' ') || e.general, kind: 'error' };
            renderSsids();
            return;
        }
        showMessage('Wi-Fi network removed from the draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        selected = null;
        await load();
        afterSave();
    }

    // ---------------------------------------------------------------- loading

    let sectionsInit = false;

    // the usual Wi-Fi is open; the link section is open only if something there is set up
    function initSections() {
        if (sectionsInit || !wifi) return;
        sectionsInit = true;
        $('wifi-sec-ap').open = true;
        $('wifi-sec-link').open = (wifi.stations || []).length > 0 || (wifi.networks || []).some(n => n.wds);
        $('wifi-sec-adapters').open = false;
    }

    function render() {
        if (!report) return;
        initSections();
        const none = report.adapters.length === 0;
        $('wifi-empty').classList.toggle('hidden', !none);
        $('wifi-body').classList.toggle('is-disabled', none);
        $('wifi-body').setAttribute('aria-disabled', String(none));
        $('wifi-notes').innerHTML = none ? '' : notes(report);
        renderAll();
    }

    function renderAll() {
        if (!report) return;
        renderSsids();
        renderAdapters();
    }

    function adoptWifi(data) {
        wifi = data;
        const fresh = (data.networks || []).map(n => ({ key: n.id, isNew: false, server: n, form: ssidFormOf(n), errors: {}, message: null, confirmDelete: false }));
        const adding = ssids.filter(c => c.isNew);
        const kept = new Map(ssids.filter(c => !c.isNew).map(c => [c.key, c]));
        ssids = fresh.map(f => {
            const old = kept.get(f.key);
            return old && !old.justSaved && ssidDirty(old) ? Object.assign(f, { form: old.form }) : f;
        }).concat(adding);
        // adapter forms follow the server unless they hold unsaved edits
        ((report && report.adapters) || []).forEach(a => {
            if (!radios[a.name] || !radioDirty(a)) radios[a.name] = { form: radioFormOf(a), errors: {}, message: radios[a.name] ? radios[a.name].message : null };
            if (!stations[a.name] || !stationDirty(a)) stations[a.name] = { form: stationFormOf(a), errors: {}, message: stations[a.name] ? stations[a.name].message : null, confirmRemove: false };
        });
    }

    async function loadReport() {
        const res = await apiFetch('/api/v1/wireless');
        if (!res || !res.ok) return false;
        const text = await res.text();
        const changed = text !== lastReport;
        lastReport = text;
        report = JSON.parse(text);
        return changed;
    }

    async function load() {
        await loadReport();
        const [w, n] = await Promise.all([apiFetch('/api/v1/wifi'), apiFetch('/api/v1/networks')]);
        if (w && w.ok) adoptWifi(await w.json());
        else showMessage('Could not load the Wi-Fi settings.', 'error');
        if (n && n.ok) localNets = (await n.json()).networks || [];
        render();
    }

    // The 10 s refresh only redraws when the adapters changed (plugged in, link up/down), so typing is not disturbed
    async function poll() {
        const changed = await loadReport();
        if (changed) { await load(); }
    }

    // ---------------------------------------------------------------- events

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('wifi-body');

        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.id === 'wifi-select') { selected = el.value; renderSsids(); return; }

            if (el.dataset.radio) {
                const a = report.adapters.find(x => x.name === el.dataset.adapter);
                if (!a) return;
                const st = radioState(a);
                const key = el.dataset.radio;
                st.form[key] = key === 'enabled' ? el.checked : el.value;
                if (key === 'band') {
                    const band = bandOf(a, el.value);
                    st.form.channel = '0';
                    if (band) st.form.width = String(defaultWidth(band));
                }
                delete st.errors[key];
                st.message = null;
                renderAdapters();
                return;
            }

            if (el.dataset.stf) {
                const a = report.adapters.find(x => x.name === el.dataset.st);
                if (!a) return;
                const st = stationState(a);
                const key = el.dataset.stf;
                if (el.type === 'radio' && !el.checked) return;
                st.form[key] = el.type === 'checkbox' ? el.checked : el.value;
                delete st.errors[key];
                st.message = null;
                renderAdapters();
                return;
            }

            const c = el.dataset.key && cardOf(el.dataset.key);
            if (!c) return;
            const field = el.dataset.field;
            const wasBoth = c.form.b24 && c.form.b5;
            c.form[field] = el.type === 'checkbox' ? el.checked : el.value;
            delete c.errors[field];
            if (field === 'b24' || field === 'b5') {
                delete c.errors.bands;
                const both = c.form.b24 && c.form.b5;
                if (both && !wasBoth) c.form.steering = true;
                if (!both) c.form.steering = false;
            }
            renderSsids();
        });

        // typing in a text box must not rebuild the card (that would drop the focus)
        body.addEventListener('input', (e) => {
            const el = e.target;
            if (el.dataset && el.dataset.stf && el.type !== 'checkbox' && el.type !== 'radio' && el.tagName !== 'SELECT') {
                const a = report.adapters.find(x => x.name === el.dataset.st);
                if (!a) return;
                const st = stationState(a);
                st.form[el.dataset.stf] = el.value;
                delete st.errors[el.dataset.stf];
                el.removeAttribute('aria-invalid');
                const msg = el.closest('.form-field').querySelector('.field-error');
                if (msg) msg.remove();
                const box = body.querySelector(`.wifi-station[data-adapter="${el.dataset.st}"]`);
                const dirty = stationDirty(a);
                const save = box && box.querySelector('.st-save'), reset = box && box.querySelector('.st-reset');
                if (save) save.disabled = !editable() || busy || !dirty;
                if (reset) reset.disabled = busy || !dirty;
                return;
            }
            const c = el.dataset && el.dataset.key && cardOf(el.dataset.key);
            if (!c || el.type === 'checkbox' || el.tagName === 'SELECT') return;
            c.form[el.dataset.field] = el.value;
            delete c.errors[el.dataset.field];
            el.removeAttribute('aria-invalid');
            const msg = el.closest('.form-field').querySelector('.field-error');
            if (msg) msg.remove();
            const card = body.querySelector(`.net-card[data-key="${c.key}"]`);
            if (!card) return;
            const dirty = ssidDirty(c);
            const save = card.querySelector('.ssid-save'), reset = card.querySelector('.ssid-reset');
            if (save) save.disabled = !editable() || busy || !dirty;
            if (reset) reset.disabled = busy || !dirty;
            const state = card.querySelector('.ssid-state');
            if (state && editable()) state.textContent = dirty && !c.isNew ? 'You have unsaved changes.' : '';
            const title = card.querySelector('.net-card-head h2');
            if (title && c.isNew) title.textContent = c.form.ssid.trim() || 'New Wi-Fi network';
            const opt = body.querySelector(`#wifi-select option[value="${c.key}"]`);
            if (opt) opt.textContent = ssidLabel(c);
        });

        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            if (btn.id === 'wifi-add') { addSsid(); return; }
            if (btn.dataset.st) {
                const a = report.adapters.find(x => x.name === btn.dataset.st);
                if (!a) return;
                if (btn.classList.contains('st-save')) saveStation(a);
                else if (btn.classList.contains('st-scan')) scanFor(a);
                else if (btn.classList.contains('st-pick')) pickNetwork(a, parseInt(btn.dataset.pick, 10));
                else if (btn.classList.contains('st-remove')) removeStation(a);
                else if (btn.classList.contains('st-reset')) { stations[a.name] = { form: stationFormOf(a), errors: {}, message: null, confirmRemove: false }; renderAdapters(); }
                else if (btn.classList.contains('st-reveal')) {
                    const st = stationState(a);
                    st.form.show = !st.form.show;
                    renderAdapters();
                    const f = document.getElementById(`st-${a.name}-passphrase`);
                    if (f) f.focus();
                }
                return;
            }
            if (btn.classList.contains('radio-save') || btn.classList.contains('radio-reset')) {
                const a = report.adapters.find(x => x.name === btn.dataset.adapter);
                if (!a) return;
                if (btn.classList.contains('radio-save')) saveRadio(a);
                else { radios[a.name] = { form: radioFormOf(a), errors: {}, message: null }; renderAdapters(); }
                return;
            }
            const c = btn.dataset.key && cardOf(btn.dataset.key);
            if (!c) return;
            if (btn.classList.contains('ssid-save')) saveSsid(c);
            else if (btn.classList.contains('ssid-delete')) removeSsid(c);
            else if (btn.classList.contains('ssid-reveal')) { c.form.show = !c.form.show; renderSsids(); const f = document.getElementById(`${c.key}-passphrase`); if (f) f.focus(); }
            else if (btn.classList.contains('ssid-reset')) { c.form = ssidFormOf(c.server); c.errors = {}; c.message = null; renderSsids(); }
            else if (btn.classList.contains('ssid-cancel')) { ssids = ssids.filter(x => x !== c); selected = null; renderSsids(); }
        });

        body.addEventListener('keydown', (e) => {
            if (e.key !== 'Enter' || e.target.tagName !== 'INPUT' || e.target.type === 'checkbox') return;
            const c = e.target.dataset.key && cardOf(e.target.dataset.key);
            if (c && ssidDirty(c) && editable() && !busy) { e.preventDefault(); saveSsid(c); }
        });
    }

    function addSsid() {
        const bands = offeredBands();
        const b24 = bands.has('2.4 GHz'), b5 = bands.has('5 GHz');
        newCount++;
        const key = 'new-' + newCount;
        ssids.push({
            key, isNew: true, server: null, errors: {}, message: null, confirmDelete: false,
            form: { ssid: '', passphrase: '', network: 'lan', security: 'wpa2-wpa3', b24, b5, steering: b24 && b5, hidden: false, isolation: false, wds: false, enabled: true, show: false },
        });
        selected = key;
        renderSsids();
        const field = document.getElementById(`${key}-ssid`);
        if (field) field.focus();
    }

    function init() {
        wire();
        showMessage('');
        report = null;
        wifi = null;
        lastReport = '';
        radios = {};
        stations = {};
        sectionsInit = false;
        ssids = [];
        selected = null;
        load();
        timer = setInterval(poll, 10000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
