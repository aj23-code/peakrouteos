// web/static/scripts/vpn.js
// VPN page, two tabs: tunnels to other sites (site to site) and remote access to this router (client VPN), both with WireGuard.
// Saving only writes a draft; the tunnels start when the change is committed. docs/vpn.md has the rules.

window.VPNModule = (function () {
    let data = null;             // GET /api/v1/vpn
    let tab = 'site';            // 'site' or 'client'
    let siteEd = null;           // { id (null: new), form, errors, message }
    let remoteEd = null;         // { form, errors, message }
    let deviceEd = null;         // { form, errors, message }
    let fileShown = null;        // { name, filename, config }: the file of a device made here, kept only in this page
    let secretShown = null;      // a pre-shared key made in the page, shown once for copying to the other router
    let confirmDelete = null;
    let busy = false;
    let timer = null;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const editable = () => !data || data.editable !== false;
    const errorText = (e) => (e ? `<p class="field-error">${escapeHtml(e)}</p>` : '');

    function showMessage(text, kind) {
        const box = $('vpn-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    async function errorOf(res, fallback) {
        if (!res) return { general: 'The router did not answer. Try again.', fields: {} };
        let err = {};
        try { err = await res.json(); } catch (e) { /* not JSON */ }
        const fields = {};
        (err.errors || []).forEach(x => { fields[x.field] = (fields[x.field] ? fields[x.field] + ' ' : '') + x.message; });
        return { general: err.error || fallback, fields };
    }

    // splits the field errors of an editor: those it has a field for, and the rest as one message
    function spread(e, known) {
        const errors = {}, other = [];
        Object.keys(e.fields).forEach(k => { if (known.includes(k)) errors[k] = e.fields[k]; else other.push(e.fields[k]); });
        return { errors, message: other.join(' ') || (Object.keys(errors).length ? 'Some settings need attention.' : e.general) };
    }

    function ago(ms) {
        const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
        if (s < 60) return 'just now';
        if (s < 3600) return `${Math.round(s / 60)} min ago`;
        if (s < 86400) return `${Math.round(s / 3600)} h ago`;
        return `${Math.round(s / 86400)} days ago`;
    }

    function statusHtml(item, kind) {
        if (!item.enabled) return '<span class="tag">Switched off</span>';
        if (item.pending) return '<span class="form-hint">Starts when you commit the change.</span>';
        const st = item.status;
        if (!st) return '<span class="form-hint">Not running.</span>';
        const traffic = (st.received || st.sent) ? ` <span class="form-hint">${formatBytes(st.received)} in, ${formatBytes(st.sent)} out.</span>` : '';
        if (st.state === 'connected') return `<span class="tag tag-ok">Connected</span> <span class="form-hint">Last heard ${ago(st.handshake)}${st.endpoint ? ' from ' + escapeHtml(st.endpoint) : ''}.</span>${traffic}`;
        if (st.state === 'idle') return `<span class="tag">Idle</span> <span class="form-hint">Last heard ${ago(st.handshake)}. ${kind === 'site' ? 'It reconnects when there is traffic.' : 'The device is not connected now.'}</span>${traffic}`;
        return `<span class="tag tag-draft">Waiting</span> <span class="form-hint">${kind === 'site' ? 'The other router has not connected yet.' : 'The device has not connected yet.'}</span>`;
    }

    function field(ed, label, id, control, key, hint, cls) {
        return `<div class="form-field${cls ? ' ' + cls : ''}"><label for="${id}">${label}</label>${control}${hint ? `<p class="form-hint">${hint}</p>` : ''}${errorText(ed.errors[key])}</div>`;
    }
    function input(ed, id, key, value, extra) {
        const lock = !editable() || busy;
        return `<input type="${extra && extra.type || 'text'}" id="${id}" data-ed="${key}" value="${escapeHtml(value)}"${extra && extra.placeholder ? ` placeholder="${escapeHtml(extra.placeholder)}"` : ''} autocomplete="off" spellcheck="false" autocapitalize="off"${extra && extra.mode ? ` inputmode="${extra.mode}"` : ''}${ed.errors[key] ? ' aria-invalid="true"' : ''}${lock ? ' disabled' : ''}>`;
    }
    function check(key, label, hint, on) {
        const lock = !editable() || busy;
        return `<label class="toggle toggle--block"><input type="checkbox" data-ed="${key}"${on ? ' checked' : ''}${lock ? ' disabled' : ''}><span><strong>${label}</strong><span class="form-hint">${hint}</span></span></label>`;
    }
    const code = (t) => `<code class="vpn-key">${escapeHtml(t)}</code>`;

    // ---------------------------------------------------------------- tabs

    function tabsHtml() {
        const t = (id, label) => `<button type="button" role="tab" class="vpn-tab${tab === id ? ' vpn-tab--on' : ''}" data-tab="${id}" aria-selected="${tab === id}">${label}</button>`;
        return `<div class="vpn-tabs" role="tablist" aria-label="Kind of VPN">${t('site', 'Site to site')}${t('client', 'Client VPN')}</div>`;
    }

    // ---------------------------------------------------------------- site to site

    function siteDetailsHtml(s) {
        const nets = (data.local_networks || []).map(n => `${escapeHtml(n.name)} ${code(n.prefix)}`).join(', ') || 'none';
        const where = data.wan_address ? `${code(data.wan_address + ':' + s.listen_port)}` : `this router's internet address, port ${code(String(s.listen_port))}`;
        return `<details class="vpn-details"><summary>Settings for the other router</summary>
            <dl class="vpn-dl">
                <dt>This router's public key</dt><dd>${code(s.public_key)}</dd>
                <dt>Connect to</dt><dd>${where}${s.endpoint ? ' <span class="form-hint">(this router calls the other one too; either side can start)</span>' : ''}</dd>
                <dt>Its address in the tunnel</dt><dd>${code(s.other_address || '')} <span class="form-hint">(this router has ${escapeHtml(s.address)})</span></dd>
                <dt>Networks to send here</dt><dd>${nets}</dd>
                <dt>Pre-shared key</dt><dd>${s.preshared_set ? 'In use (it is the same key on both sides and is not shown again).' : 'Not used.'}</dd>
            </dl></details>`;
    }

    function siteRowHtml(s) {
        const lock = !editable() || busy || !!siteEd;
        const id = escapeHtml(s.id);
        return `<div class="fw-row fw-rule${s.enabled ? '' : ' fw-rule--off'} vpn-row">
            <label class="toggle fw-switch"><input type="checkbox" class="vpn-enable" data-kind="site" data-id="${id}"${s.enabled ? ' checked' : ''}${lock ? ' disabled' : ''} aria-label="Switch on or off"></label>
            <div class="fw-main"><div class="fw-name"><strong>${escapeHtml(s.name)}</strong> <span class="tag">WireGuard</span>${s.pending ? ' <span class="tag tag-draft">Draft</span>' : ''}</div>
                <div class="form-hint fw-text">${escapeHtml(s.text)}</div>
                <div class="vpn-status">${statusHtml(s, 'site')}</div>
                ${siteDetailsHtml(s)}</div>
            <div class="fw-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost vpn-site-edit" data-id="${id}"${lock ? ' disabled' : ''}>Edit</button>
                <button type="button" class="cfg-btn cfg-btn--danger vpn-site-delete" data-id="${id}"${lock ? ' disabled' : ''}>${confirmDelete === 'site:' + s.id ? 'Click again' : 'Delete'}</button>
            </div></div>`;
    }

    function siteListHtml() {
        const lock = !editable() || busy || !!siteEd;
        return `<section class="card">
            <div class="net-card-head"><h2>Site to site tunnels</h2><span class="tag">${data.sites.length}</span>
                <button type="button" class="cfg-btn" id="vpn-site-add" style="margin-left:auto"${lock || data.sites.length >= 8 ? ' disabled' : ''}>Add a tunnel</button></div>
            <p class="form-hint">Joins this router's networks with the networks behind another router, over the internet. Each side lists the networks behind the other, and the devices on both sides reach each other as if on one network. Each tunnel is its own network in the <a href="#" data-goto="firewall-rules">firewall</a>, with its own rules.</p>
            <div class="fw-list">${data.sites.length ? data.sites.map(siteRowHtml).join('') : '<div class="fw-row fw-empty">No tunnels yet.</div>'}</div>
        </section>`;
    }

    const SITE_FIELDS = ['name', 'listen_port', 'address', 'peer_public_key', 'preshared_key', 'endpoint', 'keepalive', 'remote_networks'];

    function siteEditorHtml() {
        const ed = siteEd, f = ed.form, lock = !editable() || busy;
        const nets = (data.local_networks || []).map(n => escapeHtml(n.prefix)).join(', ');
        const saved = ed.id && f.presharedSet;
        return `<section class="card fw-editor" id="vpn-site-editor">
            <div class="net-card-head"><h2>${ed.id ? 'Edit tunnel' : 'New tunnel'}</h2><span class="form-hint">WireGuard${ed.id ? '' : ': this router makes its own key'}</span></div>
            <div class="form-grid">
                ${field(ed, 'Name', 'vpn-s-name', input(ed, 'vpn-s-name', 'name', f.name, { placeholder: 'Branch office' }), 'name')}
                ${field(ed, 'Networks behind the other router', 'vpn-s-nets', input(ed, 'vpn-s-nets', 'remote_networks', f.remote_networks, { placeholder: '192.168.50.0/24' }), 'remote_networks',
                    `Separate several with commas. They must not overlap the networks of this router${nets ? ' (' + nets + ')' : ''}.`)}
                ${field(ed, 'Public key of the other router', 'vpn-s-peer', input(ed, 'vpn-s-peer', 'peer_public_key', f.peer_public_key, { placeholder: '44 characters, ending in =' }), 'peer_public_key',
                    'Shown on the other router. After you save, this router\'s own public key is under "Settings for the other router".', 'form-field--wide')}
                ${field(ed, 'This router\'s address in the tunnel', 'vpn-s-addr', input(ed, 'vpn-s-addr', 'address', f.address, { placeholder: '10.99.1.1/30' }), 'address',
                    'The tunnel has a tiny network of its own. This router takes the first address; the other router takes the next one (10.99.1.2/30).')}
                ${field(ed, 'Address of the other router <span class="form-hint">(optional)</span>', 'vpn-s-endpoint', input(ed, 'vpn-s-endpoint', 'endpoint', f.endpoint, { placeholder: 'branch.example.com:51820' }), 'endpoint',
                    'Fill this in when this router should start the connection. Leave it empty when the other router calls this one (a router with a changing or private address should be the caller).')}
                ${field(ed, 'Port this router listens on', 'vpn-s-port', input(ed, 'vpn-s-port', 'listen_port', f.listen_port, { mode: 'numeric' }), 'listen_port', 'Each tunnel needs its own port.')}
                ${field(ed, 'Keep-alive <span class="form-hint">(seconds, optional)</span>', 'vpn-s-keep', input(ed, 'vpn-s-keep', 'keepalive', f.keepalive, { mode: 'numeric', placeholder: f.endpoint ? '25' : 'off' }), 'keepalive',
                    'Keeps a connection through a firewall or NAT alive. 25 is right when this router calls the other one.')}
                ${field(ed, 'Pre-shared key <span class="form-hint">(optional)</span>', 'vpn-s-psk', `<div class="vpn-inline">${input(ed, 'vpn-s-psk', 'preshared_key', f.preshared_key, { placeholder: saved ? 'unchanged' : 'none' })}<button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-s-psk-make"${lock ? ' disabled' : ''}>Make one</button></div>`, 'preshared_key',
                    `An extra shared secret that both routers must have. ${saved ? 'One is saved; leave this empty to keep it. ' : ''}A key made here is shown once, so that you can enter it on the other router.`, 'form-field--wide')}
            </div>
            ${secretShown ? `<div class="vpn-secret"><strong>Pre-shared key made:</strong> ${code(secretShown)} <span class="form-hint">Enter it on the other router too. It is not shown again once the draft is saved.</span></div>` : ''}
            <div class="net-access">
                ${check('internet', 'May use this router\'s internet connection', 'The other site can send its internet traffic through this router.', f.internet)}
                ${check('isolated', 'Isolated from the Local Networks', 'The other site cannot reach the networks of this router, and they cannot reach it.', f.isolated)}
                ${check('enabled', 'Switched on', 'Off keeps the tunnel here without starting it.', f.enabled)}
            </div>
            ${ed.message ? `<div class="form-message" role="alert">${escapeHtml(ed.message)}</div>` : ''}
            <div class="form-actions">
                ${ed.id ? `<button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-s-newkey"${lock ? ' disabled' : ''} title="Makes a new key for this router; the other router must then be given the new public key">New key for this router</button>` : ''}
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-s-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="vpn-s-save"${lock ? ' disabled' : ''}>${ed.id ? 'Save to draft' : 'Add tunnel'}</button>
            </div></section>`;
    }

    function openSiteEditor(id) {
        const s = id ? data.sites.find(x => x.id === id) : null;
        const sg = data.suggest || {};
        const form = s
            ? { name: s.name, remote_networks: (s.remote_networks || []).join(', '), peer_public_key: s.peer_public_key, address: s.address, endpoint: s.endpoint,
                listen_port: String(s.listen_port), keepalive: s.keepalive ? String(s.keepalive) : '', preshared_key: '', presharedSet: s.preshared_set,
                internet: s.internet, isolated: s.isolated, enabled: s.enabled }
            : { name: '', remote_networks: '', peer_public_key: '', address: sg.site_address || '', endpoint: '', listen_port: String(sg.site_port || 51820), keepalive: '',
                preshared_key: '', presharedSet: false, internet: false, isolated: false, enabled: true };
        siteEd = { id: id || null, form, errors: {}, message: null };
        secretShown = null;
        render();
        const first = $('vpn-s-name');
        if (first) first.focus();
    }

    function sitePayload(f, extra, editing) {
        const list = f.remote_networks.split(/[\s,]+/).filter(Boolean);
        const body = { name: f.name, address: f.address, peer_public_key: f.peer_public_key, endpoint: f.endpoint, remote_networks: list, internet: f.internet, isolated: f.isolated, enabled: f.enabled };
        const port = parseInt(f.listen_port, 10);
        body.listen_port = Number.isNaN(port) ? 0 : port;
        // blank: off when editing, and the usual 25 for a new tunnel that calls the other router (the router fills that in when it is left out)
        if (String(f.keepalive).trim() !== '') { const k = parseInt(f.keepalive, 10); body.keepalive = Number.isNaN(k) ? -1 : k; } else if (editing) body.keepalive = 0;
        if (f.preshared_key.trim() !== '') body.preshared_key = f.preshared_key.trim();
        return Object.assign(body, extra || {});
    }

    async function saveSite(extra) {
        const ed = siteEd, f = ed.form;
        ed.errors = {}; ed.message = null;
        busy = true; render();
        const base = '/api/v1/vpn/sites';
        const res = await apiFetch(ed.id ? `${base}/${encodeURIComponent(ed.id)}` : base, {
            method: ed.id ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(sitePayload(f, extra, !!ed.id)) });
        busy = false;
        if (!res || !res.ok) {
            const sp = spread(await errorOf(res, 'Saving failed.'), SITE_FIELDS);
            ed.errors = sp.errors; ed.message = sp.message;
            render();
            return;
        }
        const wasNew = !ed.id;
        siteEd = null; secretShown = null;
        await load();
        showMessage(wasNew ? 'Added to the draft. Give the other router this router\'s public key (under "Settings for the other router"), then review and commit from the bar at the top.' : 'Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function makePresharedKey() {
        const bytes = new Uint8Array(32);
        crypto.getRandomValues(bytes);
        let bin = '';
        bytes.forEach(b => { bin += String.fromCharCode(b); });
        const key = btoa(bin);
        siteEd.form.preshared_key = key;
        secretShown = key;
        render();
    }

    // ---------------------------------------------------------------- client VPN: the server

    function remoteCardHtml() {
        const r = data.remote;
        const lock = !editable() || busy || !!remoteEd;
        if (!r) {
            return `<section class="card">
                <div class="net-card-head"><h2>Remote access</h2><span class="tag">Not set up</span>
                    <button type="button" class="cfg-btn" id="vpn-remote-setup" style="margin-left:auto"${lock ? ' disabled' : ''}>Set up</button></div>
                <p class="form-hint">Lets laptops and phones connect to this router from anywhere, and reach the networks here as if they were at home. The router is the server: you add each device below and give it a small file to import into the WireGuard app. ${data.wan_address ? '' : 'The internet connection has no address right now.'}</p>
            </section>`;
        }
        const routes = (r.client_routes || []).map(code).join(', ');
        return `<section class="card">
            <div class="net-card-head"><h2>Remote access</h2>${r.pending ? '<span class="tag tag-draft">Draft</span>' : (r.enabled ? '<span class="tag tag-ok">On</span>' : '<span class="tag">Switched off</span>')}
                <span style="margin-left:auto;display:flex;gap:0.5rem">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-remote-edit"${lock ? ' disabled' : ''}>Edit</button>
                <button type="button" class="cfg-btn cfg-btn--danger" id="vpn-remote-delete"${lock ? ' disabled' : ''}>${confirmDelete === 'remote' ? 'Click again' : 'Remove'}</button></span></div>
            <p class="form-hint">${escapeHtml(r.text)}</p>
            <dl class="vpn-dl">
                <dt>Devices connect to</dt><dd>${r.endpoint ? code(r.endpoint.includes(':') && !r.endpoint.endsWith(']') ? r.endpoint : r.endpoint + ':' + r.listen_port) : '<span class="net-warn">Not set: devices need an address to connect to. Edit and fill it in.</span>'}</dd>
                <dt>Devices send to the router</dt><dd>${r.full_tunnel ? 'All their traffic' : (routes || 'Nothing')}${r.dns ? '; names are looked up by this router' : ''}</dd>
                <dt>The router's public key</dt><dd>${code(r.public_key)}</dd>
            </dl>
        </section>`;
    }

    const REMOTE_FIELDS = ['endpoint', 'listen_port', 'network', 'routes'];

    function remoteEditorHtml() {
        const ed = remoteEd, f = ed.form, lock = !editable() || busy;
        const wan = data.wan_address ? ` The internet connection's address right now is ${code(data.wan_address)}.` : '';
        return `<section class="card fw-editor" id="vpn-remote-editor">
            <div class="net-card-head"><h2>${data.remote ? 'Edit remote access' : 'Set up remote access'}</h2></div>
            <div class="form-grid">
                ${field(ed, 'Address devices connect to', 'vpn-r-endpoint', input(ed, 'vpn-r-endpoint', 'endpoint', f.endpoint, { placeholder: 'vpn.example.com' }), 'endpoint',
                    'A name (see <a href="#" data-goto="ddns">Dynamic DNS</a>) or an address of this router that can be reached from the internet. The port is added for you.' + wan, 'form-field--wide')}
                ${field(ed, 'Port', 'vpn-r-port', input(ed, 'vpn-r-port', 'listen_port', f.listen_port, { mode: 'numeric' }), 'listen_port', 'UDP. If this router is behind another router, forward this port to it.')}
                ${field(ed, 'Network for the devices', 'vpn-r-net', input(ed, 'vpn-r-net', 'network', f.network, { placeholder: '10.8.0.0/24' }), 'network', 'Devices get an address from here; the router takes the first one. It must not overlap any other network.')}
                ${field(ed, 'Networks devices can reach <span class="form-hint">(optional)</span>', 'vpn-r-routes', input(ed, 'vpn-r-routes', 'routes', f.routes, { placeholder: 'empty: all the networks of this router' }), 'routes',
                    'Separate with commas. Devices send only traffic for these networks through the tunnel (the rest goes the way it always did).', 'form-field--wide')}
            </div>
            <div class="net-access">
                ${check('full_tunnel', 'Send all of the device\'s traffic through this router', 'The device\'s internet traffic goes through this router too (it also needs "May use this router\'s internet connection" below).', f.full_tunnel)}
                ${check('dns', 'Devices use this router for DNS', 'Names are looked up by this router, so local names work and lookups are not seen by the café\'s network.', f.dns)}
                ${check('internet', 'May use this router\'s internet connection', 'Devices can reach the internet through this router.', f.internet)}
                ${check('isolated', 'Isolated from the Local Networks', 'Devices cannot reach the networks of this router, and they cannot reach them.', f.isolated)}
                ${check('enabled', 'Switched on', 'Off keeps the settings here without accepting connections.', f.enabled)}
            </div>
            ${ed.message ? `<div class="form-message" role="alert">${escapeHtml(ed.message)}</div>` : ''}
            <div class="form-actions">
                ${data.remote ? `<button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-r-newkey"${lock ? ' disabled' : ''} title="Makes a new key for the server; every device then needs a new file">New server key</button>` : ''}
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-r-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="vpn-r-save"${lock ? ' disabled' : ''}>${data.remote ? 'Save to draft' : 'Set up'}</button>
            </div></section>`;
    }

    function openRemoteEditor() {
        const r = data.remote, sg = data.suggest || {};
        const form = r
            ? { endpoint: r.endpoint, listen_port: String(r.listen_port), network: r.network, routes: (r.routes || []).join(', '), full_tunnel: r.full_tunnel, dns: r.dns, internet: r.internet, isolated: r.isolated, enabled: r.enabled }
            : { endpoint: data.wan_address || '', listen_port: String(sg.remote_port || 51821), network: sg.remote_network || '10.8.0.0/24', routes: '', full_tunnel: false, dns: true, internet: false, isolated: false, enabled: true };
        remoteEd = { form, errors: {}, message: null };
        render();
        const first = $('vpn-r-endpoint');
        if (first) first.focus();
    }

    async function saveRemote(extra) {
        const ed = remoteEd, f = ed.form;
        ed.errors = {}; ed.message = null;
        const port = parseInt(f.listen_port, 10);
        const body = Object.assign({ endpoint: f.endpoint, listen_port: Number.isNaN(port) ? 0 : port, network: f.network, routes: f.routes.split(/[\s,]+/).filter(Boolean),
            full_tunnel: f.full_tunnel, dns: f.dns, internet: f.internet, isolated: f.isolated, enabled: f.enabled }, extra || {});
        busy = true; render();
        const res = await apiFetch('/api/v1/vpn/remote', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        busy = false;
        if (!res || !res.ok) {
            const sp = spread(await errorOf(res, 'Saving failed.'), REMOTE_FIELDS);
            ed.errors = sp.errors; ed.message = sp.message;
            render();
            return;
        }
        const was = !!data.remote;
        remoteEd = null;
        await load();
        showMessage(extra && extra.new_key ? 'A new server key is in the draft. Every device needs a new file once it is committed.' : (was ? 'Saved as a draft. Review and commit it from the bar at the top when you are ready.' : 'Remote access is in the draft. Add the devices, then review and commit from the bar at the top.'), 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    // ---------------------------------------------------------------- client VPN: the devices

    function deviceRowHtml(c) {
        const lock = !editable() || busy || !!deviceEd;
        const id = escapeHtml(c.id);
        return `<div class="fw-row fw-rule${c.enabled ? '' : ' fw-rule--off'} vpn-row">
            <label class="toggle fw-switch"><input type="checkbox" class="vpn-enable" data-kind="client" data-id="${id}"${c.enabled ? ' checked' : ''}${lock ? ' disabled' : ''} aria-label="Switch on or off"></label>
            <div class="fw-main"><div class="fw-name"><strong>${escapeHtml(c.name)}</strong>${c.pending ? ' <span class="tag tag-draft">Draft</span>' : ''}</div>
                <div class="form-hint fw-text">${escapeHtml(c.text)}</div>
                <div class="vpn-status">${statusHtml(c, 'client')}</div></div>
            <div class="fw-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost vpn-dev-rename" data-id="${id}"${lock ? ' disabled' : ''}>Rename</button>
                <button type="button" class="cfg-btn cfg-btn--ghost vpn-dev-newkey" data-id="${id}"${lock ? ' disabled' : ''} title="Makes new keys and a new file for this device; the old file stops working once committed">New file</button>
                <button type="button" class="cfg-btn cfg-btn--danger vpn-dev-delete" data-id="${id}"${lock ? ' disabled' : ''}>${confirmDelete === 'client:' + c.id ? 'Click again' : 'Remove'}</button>
            </div></div>`;
    }

    function devicesCardHtml() {
        const r = data.remote;
        if (!r) return '';
        const lock = !editable() || busy || !!deviceEd;
        const list = r.clients || [];
        return `<section class="card">
            <div class="net-card-head"><h2>Devices</h2><span class="tag">${list.length}</span>
                <button type="button" class="cfg-btn" id="vpn-dev-add" style="margin-left:auto"${lock || list.length >= 100 ? ' disabled' : ''}>Add a device</button></div>
            <p class="form-hint">Each laptop or phone gets its own keys and address, so one can be removed without touching the others. After adding a device you get a file for the WireGuard app on it (the private key is in that file, shown only once).</p>
            <div class="fw-list">${list.length ? list.map(deviceRowHtml).join('') : '<div class="fw-row fw-empty">No devices yet.</div>'}</div>
        </section>`;
    }

    function deviceEditorHtml() {
        const ed = deviceEd, f = ed.form, lock = !editable() || busy;
        const renaming = !!ed.id;
        return `<section class="card fw-editor" id="vpn-dev-editor">
            <div class="net-card-head"><h2>${renaming ? 'Rename device' : 'New device'}</h2></div>
            <div class="form-grid">
                ${field(ed, 'Name', 'vpn-d-name', input(ed, 'vpn-d-name', 'name', f.name, { placeholder: 'Alec phone' }), 'name', 'Who or what it is; letters, digits, spaces and . _ ( ) + -')}
                ${renaming ? '' : field(ed, 'Public key of the device <span class="form-hint">(optional)</span>', 'vpn-d-key', input(ed, 'vpn-d-key', 'public_key', f.public_key, { placeholder: 'leave empty to let the router make the keys' }), 'public_key',
                    'Only if the device already made its own keys. Then the router never knows its private key, and you put that into the file yourself.', 'form-field--wide')}
            </div>
            ${renaming ? '' : `<div class="net-access">${check('preshared', 'Add an extra shared secret', 'A pre-shared key in the file adds protection for the future. The file has it.', f.preshared)}</div>`}
            ${ed.message ? `<div class="form-message" role="alert">${escapeHtml(ed.message)}</div>` : ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-d-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="vpn-d-save"${lock ? ' disabled' : ''}>${renaming ? 'Save to draft' : 'Add device'}</button>
            </div></section>`;
    }

    function fileCardHtml() {
        const f = fileShown;
        return `<section class="card vpn-file" id="vpn-file">
            <div class="net-card-head"><h2>File for ${escapeHtml(f.name)}</h2></div>
            ${f.own ? '<p class="form-hint">The device made its own keys, so this router does not know its private key. Replace the marked line with it, then import the file into the WireGuard app on the device. The device can connect once the change is committed.</p>' :
            `<p class="form-hint"><strong>Keep this file safe.</strong> It holds the device's private key, which this router does not keep: this is the only time it can be shown. Import it into the WireGuard app on the device (<em>Add tunnel, Import from file</em>), then delete it. The device can connect once the change is committed.</p>`}
            <pre class="vpn-conf">${escapeHtml(f.config)}</pre>
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-file-copy">Copy</button>
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-file-download">Download ${escapeHtml(f.filename)}</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="vpn-file-done">I have it, hide this</button>
            </div></section>`;
    }

    function openDeviceEditor(id) {
        const c = id ? data.remote.clients.find(x => x.id === id) : null;
        deviceEd = { id: id || null, form: { name: c ? c.name : '', public_key: '', preshared: false }, errors: {}, message: null };
        render();
        const first = $('vpn-d-name');
        if (first) first.focus();
    }

    const DEVICE_FIELDS = ['name', 'public_key', 'endpoint'];

    async function saveDevice() {
        const ed = deviceEd, f = ed.form;
        ed.errors = {}; ed.message = null;
        const body = { name: f.name };
        if (!ed.id) {
            if (f.public_key.trim() !== '') body.public_key = f.public_key.trim();
            if (f.preshared) body.preshared = true;
        }
        busy = true; render();
        const base = '/api/v1/vpn/remote/clients';
        const res = await apiFetch(ed.id ? `${base}/${encodeURIComponent(ed.id)}` : base, {
            method: ed.id ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        busy = false;
        if (!res || !res.ok) {
            const sp = spread(await errorOf(res, 'Saving failed.'), DEVICE_FIELDS);
            ed.errors = sp.errors; ed.message = sp.message;
            render();
            return;
        }
        const made = await res.json();
        const wasNew = !ed.id;
        deviceEd = null;
        fileShown = made.file ? { name: made.client.name, filename: made.file.filename, config: made.file.config } : null;
        if (!made.file && wasNew) {
            // the device has its own keys: the file is the same except for the place where its private key goes
            const t = await apiFetch(`/api/v1/vpn/remote/clients/${encodeURIComponent(made.client.id)}/file`);
            if (t && t.ok) { const tf = await t.json(); fileShown = { name: made.client.name, filename: tf.filename, config: tf.config, own: true }; }
        }
        await load();
        let note = wasNew ? 'Added to the draft. Review and commit it from the bar at the top; the device can connect then.' : 'Saved as a draft. Review and commit it from the bar at the top when you are ready.';
        if (wasNew && !made.file) note += ' The device has its own keys: the file below has a place for its private key.';
        showMessage(note, 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
        const card = $('vpn-file');
        if (card) card.scrollIntoView({ block: 'nearest' });
    }

    async function newDeviceFile(id) {
        busy = true; render();
        const res = await apiFetch(`/api/v1/vpn/remote/clients/${encodeURIComponent(id)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ new_key: true }) });
        busy = false;
        if (!res || !res.ok) { showMessage((await errorOf(res, 'It could not be done.')).general, 'error'); await load(); return; }
        const made = await res.json();
        fileShown = made.file ? { name: made.client.name, filename: made.file.filename, config: made.file.config } : null;
        await load();
        showMessage('New keys are in the draft. The old file stops working once you commit; import the new one below on the device.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    // ---------------------------------------------------------------- loading and the small actions

    function render() {
        if (!data) return;
        let body;
        if (tab === 'site') {
            body = siteListHtml() + (siteEd ? siteEditorHtml() : '');
        } else {
            body = remoteCardHtml() + (remoteEd ? remoteEditorHtml() : '') + devicesCardHtml() + (deviceEd ? deviceEditorHtml() : '') + (fileShown ? fileCardHtml() : '');
        }
        $('vpn-body').innerHTML = tabsHtml() + body +
            (!editable() ? '<p class="form-hint">A change is being applied or waits for confirmation: this page cannot be edited right now.</p>' : '');
    }

    async function load() {
        const res = await apiFetch('/api/v1/vpn');
        if (!res || !res.ok) { showMessage('Could not load the VPN settings.', 'error'); return; }
        data = await res.json();
        render();
    }

    async function simple(url, method, body, okText) {
        busy = true; render();
        const res = await apiFetch(url, { method, headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'The change could not be saved.'); showMessage(Object.values(e.fields).join(' ') || e.general, 'error'); await load(); return; }
        await load();
        showMessage(okText, 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function remove(key, url, okText) {
        if (confirmDelete !== key) {
            confirmDelete = key; render();
            setTimeout(() => { if (confirmDelete === key) { confirmDelete = null; render(); } }, 4000);
            return;
        }
        confirmDelete = null;
        simple(url, 'DELETE', null, okText);
    }

    async function copyFile() {
        try { await navigator.clipboard.writeText(fileShown.config); showMessage('Copied.', 'ok'); }
        catch (e) { showMessage('Copying was not allowed here: select the text and copy it, or use Download.', 'error'); }
    }

    function downloadFile() {
        const url = URL.createObjectURL(new Blob([fileShown.config], { type: 'text/plain' }));
        const a = document.createElement('a');
        a.href = url; a.download = fileShown.filename;
        document.body.appendChild(a); a.click(); a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
    }

    // ---------------------------------------------------------------- events

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('vpn-body');
        body.addEventListener('click', (e) => {
            const tb = e.target.closest('[data-tab]');
            if (tb) { tab = tb.dataset.tab; showMessage(''); render(); return; }
            const btn = e.target.closest('button');
            if (!btn) return;
            const id = btn.dataset.id;
            switch (true) {
                case btn.id === 'vpn-site-add': openSiteEditor(null); break;
                case btn.id === 'vpn-s-save': saveSite(); break;
                case btn.id === 'vpn-s-newkey': saveSite({ new_key: true }); break;
                case btn.id === 'vpn-s-cancel': siteEd = null; secretShown = null; render(); break;
                case btn.id === 'vpn-s-psk-make': makePresharedKey(); break;
                case btn.classList.contains('vpn-site-edit'): openSiteEditor(id); break;
                case btn.classList.contains('vpn-site-delete'): remove('site:' + id, `/api/v1/vpn/sites/${encodeURIComponent(id)}`, 'Removed from the draft. Review and commit it from the bar at the top when you are ready.'); break;
                case btn.id === 'vpn-remote-setup' || btn.id === 'vpn-remote-edit': openRemoteEditor(); break;
                case btn.id === 'vpn-remote-delete': remove('remote', '/api/v1/vpn/remote', 'Remote access and its devices are removed from the draft. Review and commit it from the bar at the top when you are ready.'); break;
                case btn.id === 'vpn-r-save': saveRemote(); break;
                case btn.id === 'vpn-r-newkey': saveRemote({ new_key: true }); break;
                case btn.id === 'vpn-r-cancel': remoteEd = null; render(); break;
                case btn.id === 'vpn-dev-add': openDeviceEditor(null); break;
                case btn.id === 'vpn-d-save': saveDevice(); break;
                case btn.id === 'vpn-d-cancel': deviceEd = null; render(); break;
                case btn.classList.contains('vpn-dev-rename'): openDeviceEditor(id); break;
                case btn.classList.contains('vpn-dev-newkey'): newDeviceFile(id); break;
                case btn.classList.contains('vpn-dev-delete'): remove('client:' + id, `/api/v1/vpn/remote/clients/${encodeURIComponent(id)}`, 'Removed from the draft. Review and commit it from the bar at the top when you are ready.'); break;
                case btn.id === 'vpn-file-copy': copyFile(); break;
                case btn.id === 'vpn-file-download': downloadFile(); break;
                case btn.id === 'vpn-file-done': fileShown = null; render(); break;
            }
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.classList.contains('vpn-enable')) {
                const url = el.dataset.kind === 'site' ? `/api/v1/vpn/sites/${encodeURIComponent(el.dataset.id)}` : `/api/v1/vpn/remote/clients/${encodeURIComponent(el.dataset.id)}`;
                simple(url, 'PUT', { enabled: el.checked }, el.checked ? 'Switched on in the draft.' : 'Switched off in the draft.');
                return;
            }
            const ed = siteEd || remoteEd || deviceEd;
            if (!ed || !el.dataset.ed) return;
            if (el.type === 'checkbox') ed.form[el.dataset.ed] = el.checked;
        });
        body.addEventListener('input', (e) => {
            const el = e.target;
            const ed = siteEd || remoteEd || deviceEd;
            if (!ed || !el.dataset.ed || el.type === 'checkbox') return;
            ed.form[el.dataset.ed] = el.value;
            delete ed.errors[el.dataset.ed];
            el.removeAttribute('aria-invalid');
            el.closest('.form-field')?.querySelector('.field-error')?.remove();
        });
        body.addEventListener('keydown', (e) => {
            const ed = siteEd || remoteEd || deviceEd;
            if (e.key === 'Enter' && ed && e.target.dataset.ed && e.target.tagName === 'INPUT' && e.target.type !== 'checkbox' && !busy) {
                e.preventDefault();
                if (siteEd) saveSite(); else if (remoteEd) saveRemote(); else saveDevice();
            }
        });
    }

    function init() {
        wire();
        showMessage('');
        data = null; siteEd = remoteEd = deviceEd = null; fileShown = null; secretShown = null; confirmDelete = null; busy = false;
        load();
        // who is connected changes by itself: look again now and then (not while something is being edited)
        timer = setInterval(async () => {
            if (siteEd || remoteEd || deviceEd || busy) return;
            const res = await apiFetch('/api/v1/vpn');
            if (res && res.ok && !siteEd && !remoteEd && !deviceEd && !busy) { data = await res.json(); render(); }
        }, 10000);
        return () => { clearInterval(timer); timer = null; fileShown = null; secretShown = null; };
    }

    return { init };
})();
