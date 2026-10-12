// web/static/scripts/vpn.js
// VPN page, two tabs: tunnels to other sites (site to site) and remote access to this router (client VPN), with WireGuard or OpenVPN.
// Saving only writes a draft; the tunnels start when the change is committed. docs/vpn.md has the rules.

window.VPNModule = (function () {
    let data = null;             // GET /api/v1/vpn
    let tab = 'site';            // 'site' or 'client'
    let siteEd = null;           // { id (null: new), form, errors, message }; form.engine and form.role pick the kind
    let remoteEd = null;         // { engine, form, errors, message }
    let deviceEd = null;         // { engine, id, form, errors, message }
    let fileShown = null;        // { title, filename, config, kind }: a file made here, kept only in this page
    let secretShown = null;      // a pre-shared key made in the page, shown once for copying to the other router
    let confirmDelete = null;
    let busy = false;
    let timer = null;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const editable = () => !data || data.editable !== false;
    const errorText = (e) => (e ? `<p class="field-error">${escapeHtml(e)}</p>` : '');
    const anyEditor = () => siteEd || remoteEd || deviceEd;
    const ENGINE_NAME = { wireguard: 'WireGuard', openvpn: 'OpenVPN' };

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
        (err.errors || []).forEach(x => { fields[x.field] = (fields[x.field] ? fields[x.field] + '; ' : '') + x.message; });
        return { general: err.error || fallback, fields };
    }

    // splits the field errors of an editor: those it has a field for (some under another name), and the rest as one message
    function spread(e, known, rename) {
        const errors = {}, other = [];
        Object.keys(e.fields).forEach(k => {
            const name = (rename && rename[k]) || k;
            if (known.includes(name)) errors[name] = (errors[name] ? errors[name] + '; ' : '') + e.fields[k]; else other.push(e.fields[k]);
        });
        return { errors, message: other.join(' ') || (Object.keys(errors).length ? 'Some settings need attention.' : e.general) };
    }

    function ago(ms) {
        const s = Math.max(0, Math.round((Date.now() - ms) / 1000));
        if (s < 60) return 'just now';
        if (s < 3600) return `${Math.round(s / 60)} min ago`;
        if (s < 86400) return `${Math.round(s / 3600)} h ago`;
        return `${Math.round(s / 86400)} days ago`;
    }
    const dateOf = (ms) => new Date(ms).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });

    function statusHtml(item, kind, engine) {
        if (!item.enabled) return '<span class="tag">Switched off</span>';
        if (item.pending) return '<span class="form-hint">Starts when you commit the change.</span>';
        const st = item.status;
        if (!st) return '<span class="form-hint">Not running.</span>';
        const traffic = (st.received || st.sent) ? ` <span class="form-hint">${formatBytes(st.received)} in, ${formatBytes(st.sent)} out.</span>` : '';
        if (st.state === 'connected') {
            const since = st.handshake ? (engine === 'openvpn' ? `Connected ${ago(st.handshake)}` : `Last heard ${ago(st.handshake)}`) : 'Connected';
            return `<span class="tag tag-ok">Connected</span> <span class="form-hint">${since}${st.endpoint ? ' from ' + escapeHtml(st.endpoint) : ''}.</span>${traffic}`;
        }
        if (st.state === 'idle') return `<span class="tag">Idle</span> <span class="form-hint">Last heard ${ago(st.handshake)}. ${kind === 'site' ? 'It reconnects when there is traffic.' : 'The device is not connected now.'}</span>${traffic}`;
        if (st.state === 'offline') return `<span class="tag tag-draft">Not connected</span> <span class="form-hint">${kind === 'site' ? 'The other router is not connected.' : 'The device is not connected now.'}</span>`;
        return `<span class="tag tag-draft">Waiting</span> <span class="form-hint">${kind === 'site' ? 'The other router has not connected yet.' : 'The device has not connected yet.'}</span>`;
    }

    function field(ed, label, id, control, key, hint, cls) {
        return `<div class="form-field${cls ? ' ' + cls : ''}"><label for="${id}">${label}</label>${control}${hint ? `<p class="form-hint">${hint}</p>` : ''}${errorText(ed.errors[key])}</div>`;
    }
    function input(ed, id, key, value, extra) {
        const lock = !editable() || busy;
        return `<input type="${extra && extra.type || 'text'}" id="${id}" data-ed="${key}" value="${escapeHtml(value)}"${extra && extra.placeholder ? ` placeholder="${escapeHtml(extra.placeholder)}"` : ''} autocomplete="off" spellcheck="false" autocapitalize="off"${extra && extra.mode ? ` inputmode="${extra.mode}"` : ''}${ed.errors[key] ? ' aria-invalid="true"' : ''}${lock ? ' disabled' : ''}>`;
    }
    function select(ed, id, key, value, options) {
        const lock = !editable() || busy;
        return `<select id="${id}" class="role-select" data-ed="${key}"${lock ? ' disabled' : ''}>${options.map(o => `<option value="${escapeHtml(o[0])}"${o[0] === value ? ' selected' : ''}>${escapeHtml(o[1])}</option>`).join('')}</select>`;
    }
    function check(key, label, hint, on) {
        const lock = !editable() || busy;
        return `<label class="toggle toggle--block"><input type="checkbox" data-ed="${key}"${on ? ' checked' : ''}${lock ? ' disabled' : ''}><span><strong>${label}</strong><span class="form-hint">${hint}</span></span></label>`;
    }
    const code = (t) => `<code class="vpn-key">${escapeHtml(t)}</code>`;
    const PROTOS = [['udp', 'UDP (the usual choice)'], ['tcp', 'TCP (gets through firewalls that block UDP, slower)']];

    // ---------------------------------------------------------------- tabs

    function tabsHtml() {
        const t = (id, label) => `<button type="button" role="tab" class="vpn-tab${tab === id ? ' vpn-tab--on' : ''}" data-tab="${id}" aria-selected="${tab === id}">${label}</button>`;
        return `<div class="vpn-tabs" role="tablist" aria-label="Kind of VPN">${t('site', 'Site to site')}${t('client', 'Client VPN')}</div>`;
    }

    // ---------------------------------------------------------------- site to site: the list

    function siteDetailsHtml(s) {
        const nets = (data.local_networks || []).map(n => `${escapeHtml(n.name)} ${code(n.prefix)}`).join(', ') || 'none';
        if (s.engine === 'openvpn') {
            if (s.role === 'client') {
                return `<details class="vpn-details"><summary>Details</summary><dl class="vpn-dl">
                    <dt>Connects to</dt><dd>${code(s.remote)} <span class="form-hint">(${escapeHtml((s.proto || '').toUpperCase())})</span></dd>
                    ${s.address ? `<dt>Its address in the tunnel</dt><dd>${code(s.address)}</dd>` : ''}
                    <dt>Certificate</dt><dd>${s.cert_expires ? `valid until ${escapeHtml(dateOf(s.cert_expires))}${s.cert_expires < Date.now() + 60 * 864e5 ? ' <span class="net-warn">Ask the other router for a new file before then.</span>' : ''}` : 'unknown'}</dd>
                </dl></details>`;
            }
            return `<details class="vpn-details"><summary>Settings for the other router</summary><dl class="vpn-dl">
                <dt>What it needs</dt><dd>The file made for it when you added the tunnel (or pressed <em>New file for the other router</em>). It holds the keys, the address to connect to and the networks to send here.</dd>
                <dt>Connects to</dt><dd>${code(s.remote || '')} <span class="form-hint">(${escapeHtml((s.proto || '').toUpperCase())})</span></dd>
                <dt>Its address in the tunnel</dt><dd>${code(s.other_address || '')} <span class="form-hint">(this router has ${escapeHtml(s.address)})</span></dd>
                <dt>Networks to send here</dt><dd>${nets}</dd>
            </dl></details>`;
        }
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
        const lock = !editable() || busy || !!anyEditor();
        const id = escapeHtml(s.id);
        const tag = ENGINE_NAME[s.engine] || s.engine;
        return `<div class="fw-row fw-rule${s.enabled ? '' : ' fw-rule--off'} vpn-row">
            <label class="toggle fw-switch"><input type="checkbox" class="vpn-enable" data-kind="site" data-id="${id}"${s.enabled ? ' checked' : ''}${lock ? ' disabled' : ''} aria-label="Switch on or off"></label>
            <div class="fw-main"><div class="fw-name"><strong>${escapeHtml(s.name)}</strong> <span class="tag">${escapeHtml(tag)}</span>${s.pending ? ' <span class="tag tag-draft">Draft</span>' : ''}</div>
                <div class="form-hint fw-text">${escapeHtml(s.text)}</div>
                <div class="vpn-status">${statusHtml(s, 'site', s.engine)}</div>
                ${siteDetailsHtml(s)}</div>
            <div class="fw-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost vpn-site-edit" data-id="${id}"${lock ? ' disabled' : ''}>Edit</button>
                <button type="button" class="cfg-btn cfg-btn--danger vpn-site-delete" data-id="${id}"${lock ? ' disabled' : ''}>${confirmDelete === 'site:' + s.id ? 'Click again' : 'Delete'}</button>
            </div></div>`;
    }

    function siteListHtml() {
        const lock = !editable() || busy || !!anyEditor();
        return `<section class="card">
            <div class="net-card-head"><h2>Site to site tunnels</h2><span class="tag">${data.sites.length}</span>
                <button type="button" class="cfg-btn" id="vpn-site-add" style="margin-left:auto"${lock || data.sites.length >= 8 ? ' disabled' : ''}>Add a tunnel</button></div>
            <p class="form-hint">Joins this router's networks with the networks behind another router, over the internet. Each side lists the networks behind the other, and the devices on both sides reach each other as if on one network. Each tunnel is its own network in the <a href="#" data-goto="firewall-rules">firewall</a>, with its own rules. WireGuard is the simpler and faster choice; OpenVPN is for when the other side speaks only that.</p>
            <div class="fw-list">${data.sites.length ? data.sites.map(siteRowHtml).join('') : '<div class="fw-row fw-empty">No tunnels yet.</div>'}</div>
        </section>`;
    }

    // ---------------------------------------------------------------- site to site: the editor

    const SITE_FIELDS = ['name', 'listen_port', 'address', 'peer_public_key', 'preshared_key', 'endpoint', 'keepalive', 'remote_networks', 'remote', 'proto', 'file'];
    const SITE_RENAME = { 'openvpn.remote': 'remote', 'openvpn.proto': 'proto', 'openvpn.cert': 'file', 'openvpn.key': 'file', 'openvpn.ca_cert': 'file', 'openvpn.tls_crypt': 'file', 'openvpn.role': 'file' };

    function kindPickerHtml(ed, lock) {
        const f = ed.form;
        const opt = (value, label, hint, name) => `<label class="radio"><input type="radio" name="${name}" value="${value}" data-ed="${name}"${f[name] === value ? ' checked' : ''}${lock ? ' disabled' : ''}> ${label}<span class="form-hint">${hint}</span></label>`;
        return `<div class="radio-row" role="radiogroup" aria-label="Technology">
                ${opt('wireguard', 'WireGuard', 'Fast and simple: each router has a key and gives the other its public key.', 'engine')}
                ${opt('openvpn', 'OpenVPN', 'Certificates made by this router. One router makes a file, the other imports it.', 'engine')}
            </div>
            ${f.engine === 'openvpn' ? `<div class="radio-row" role="radiogroup" aria-label="Which side">
                ${opt('server', 'This router waits', 'It makes a file for the other router to import. The other router connects to this one.', 'role')}
                ${opt('client', 'This router connects', 'The other router made a file; import it here. This router connects to the other one.', 'role')}
            </div>` : ''}`;
    }

    function siteEditorHtml() {
        const ed = siteEd, f = ed.form, lock = !editable() || busy;
        const nets = (data.local_networks || []).map(n => escapeHtml(n.prefix)).join(', ');
        const netsHint = `Separate several with commas. They must not overlap the networks of this router${nets ? ' (' + nets + ')' : ''}.`;
        const isOV = f.engine === 'openvpn';
        const parts = [];
        parts.push(field(ed, 'Name', 'vpn-s-name', input(ed, 'vpn-s-name', 'name', f.name, { placeholder: 'Branch office' }), 'name'));
        parts.push(field(ed, 'Networks behind the other router', 'vpn-s-nets', input(ed, 'vpn-s-nets', 'remote_networks', f.remote_networks, { placeholder: '192.168.50.0/24' }), 'remote_networks',
            netsHint + (isOV && f.role === 'client' && !ed.id ? ' Left empty, the networks the file names are used.' : '')));
        if (!isOV) {
            const saved = ed.id && f.presharedSet;
            parts.push(field(ed, 'Public key of the other router', 'vpn-s-peer', input(ed, 'vpn-s-peer', 'peer_public_key', f.peer_public_key, { placeholder: '44 characters, ending in =' }), 'peer_public_key',
                'Shown on the other router. After you save, this router\'s own public key is under "Settings for the other router".', 'form-field--wide'));
            parts.push(field(ed, 'This router\'s address in the tunnel', 'vpn-s-addr', input(ed, 'vpn-s-addr', 'address', f.address, { placeholder: '10.99.1.1/30' }), 'address',
                'The tunnel has a tiny network of its own. This router takes the first address; the other router takes the next one (10.99.1.2/30).'));
            parts.push(field(ed, 'Address of the other router <span class="form-hint">(optional)</span>', 'vpn-s-endpoint', input(ed, 'vpn-s-endpoint', 'endpoint', f.endpoint, { placeholder: 'branch.example.com:51820' }), 'endpoint',
                'Fill this in when this router should start the connection. Leave it empty when the other router calls this one (a router with a changing or private address should be the caller).'));
            parts.push(field(ed, 'Port this router listens on', 'vpn-s-port', input(ed, 'vpn-s-port', 'listen_port', f.listen_port, { mode: 'numeric' }), 'listen_port', 'Each tunnel needs its own port (UDP).'));
            parts.push(field(ed, 'Keep-alive <span class="form-hint">(seconds, optional)</span>', 'vpn-s-keep', input(ed, 'vpn-s-keep', 'keepalive', f.keepalive, { mode: 'numeric', placeholder: f.endpoint ? '25' : 'off' }), 'keepalive',
                'Keeps a connection through a firewall or NAT alive. 25 is right when this router calls the other one.'));
            parts.push(field(ed, 'Pre-shared key <span class="form-hint">(optional)</span>', 'vpn-s-psk', `<div class="vpn-inline">${input(ed, 'vpn-s-psk', 'preshared_key', f.preshared_key, { placeholder: saved ? 'unchanged' : 'none' })}<button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-s-psk-make"${lock ? ' disabled' : ''}>Make one</button></div>`, 'preshared_key',
                `An extra shared secret that both routers must have. ${saved ? 'One is saved; leave this empty to keep it. ' : ''}A key made here is shown once, so that you can enter it on the other router.`, 'form-field--wide'));
        } else if (f.role === 'server') {
            parts.push(field(ed, 'Address the other router connects to', 'vpn-s-remote', input(ed, 'vpn-s-remote', 'remote', f.remote, { placeholder: 'router.example.com:1195' }), 'remote',
                'A name (see <a href="#" data-goto="ddns">Dynamic DNS</a>) or an address of this router that can be reached from the internet, and the port. It goes into the file for the other router.' + (data.wan_address ? ` The internet connection's address right now is ${code(data.wan_address)}.` : ''), 'form-field--wide'));
            parts.push(field(ed, 'Protocol', 'vpn-s-proto', select(ed, 'vpn-s-proto', 'proto', f.proto, PROTOS), 'proto'));
            parts.push(field(ed, 'Port this router listens on', 'vpn-s-port', input(ed, 'vpn-s-port', 'listen_port', f.listen_port, { mode: 'numeric' }), 'listen_port', 'Each tunnel needs its own port.'));
            parts.push(field(ed, 'This router\'s address in the tunnel', 'vpn-s-addr', input(ed, 'vpn-s-addr', 'address', f.address, { placeholder: '10.98.1.1/29' }), 'address',
                'The tunnel has a small network of its own (at least a /29). This router takes the first address; the other router gets the next one.'));
        } else {
            parts.push(field(ed, ed.id ? 'Replace the file <span class="form-hint">(optional)</span>' : 'The file from the other router', 'vpn-s-file',
                `<textarea id="vpn-s-file" class="vpn-textarea" data-ed="file" rows="6" spellcheck="false" placeholder="Paste the contents of the .ovpn file here, or choose the file below"${ed.errors.file ? ' aria-invalid="true"' : ''}${lock ? ' disabled' : ''}>${escapeHtml(f.file)}</textarea>
                 <div class="vpn-inline"><input type="file" id="vpn-s-file-pick" accept=".ovpn,.conf,.txt,text/plain"${lock ? ' disabled' : ''}></div>`, 'file',
                ed.id ? 'Only if the other router made a new file (for example when its certificate was renewed).' : 'The other router makes it when it adds its side of the tunnel (VPN, Site to site, OpenVPN, <em>This router waits</em>).', 'form-field--wide'));
            if (ed.id) parts.push(`<p class="form-hint">Connects to ${code(f.remote)} (${escapeHtml((f.proto || '').toUpperCase())}).</p>`);
        }
        return `<section class="card fw-editor" id="vpn-site-editor">
            <div class="net-card-head"><h2>${ed.id ? 'Edit tunnel' : 'New tunnel'}</h2><span class="form-hint">${ENGINE_NAME[f.engine]}${ed.id ? '' : (isOV && f.role === 'server' ? ': this router makes the certificates' : (isOV ? '' : ': this router makes its own key'))}</span></div>
            ${ed.id ? '' : kindPickerHtml(ed, lock)}
            <div class="form-grid">${parts.join('')}</div>
            ${secretShown ? `<div class="vpn-secret"><strong>Pre-shared key made:</strong> ${code(secretShown)} <span class="form-hint">Enter it on the other router too. It is not shown again once the draft is saved.</span></div>` : ''}
            <div class="net-access">
                ${check('internet', 'May use this router\'s internet connection', 'The other site can send its internet traffic through this router.', f.internet)}
                ${check('isolated', 'Isolated from the Local Networks', 'The other site cannot reach the networks of this router, and they cannot reach it.', f.isolated)}
                ${check('enabled', 'Switched on', 'Off keeps the tunnel here without starting it.', f.enabled)}
            </div>
            ${ed.message ? `<div class="form-message" role="alert">${escapeHtml(ed.message)}</div>` : ''}
            <div class="form-actions">
                ${ed.id && !isOV ? `<button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-s-newkey"${lock ? ' disabled' : ''} title="Makes a new key for this router; the other router must then be given the new public key">New key for this router</button>` : ''}
                ${ed.id && isOV && f.role === 'server' ? `<button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-s-newfile"${lock ? ' disabled' : ''} title="Makes a new certificate for the other router and a new file; the old file stops working once committed">New file for the other router</button>` : ''}
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-s-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="vpn-s-save"${lock ? ' disabled' : ''}>${ed.id ? 'Save to draft' : 'Add tunnel'}</button>
            </div></section>`;
    }

    function openSiteEditor(id) {
        const s = id ? data.sites.find(x => x.id === id) : null;
        const sg = data.suggest || {};
        let form;
        if (s) {
            form = { engine: s.engine, role: s.role || '', name: s.name, remote_networks: (s.remote_networks || []).join(', '), peer_public_key: s.peer_public_key, address: s.address, endpoint: s.endpoint,
                listen_port: String(s.listen_port || ''), keepalive: s.keepalive ? String(s.keepalive) : '', preshared_key: '', presharedSet: s.preshared_set,
                remote: s.remote || '', proto: s.proto || 'udp', file: '', internet: s.internet, isolated: s.isolated, enabled: s.enabled };
        } else {
            form = { engine: 'wireguard', role: 'server', name: '', remote_networks: '', peer_public_key: '', address: sg.site_address || '', endpoint: '', listen_port: String(sg.site_port || 51820), keepalive: '',
                preshared_key: '', presharedSet: false, remote: '', proto: 'udp', file: '', internet: false, isolated: false, enabled: true };
        }
        siteEd = { id: id || null, form, errors: {}, message: null };
        secretShown = null;
        render();
        const first = $('vpn-s-name');
        if (first) first.focus();
    }

    // choosing the technology or the side changes what the form asks for (and what it starts with)
    function kindChanged() {
        const f = siteEd.form, sg = data.suggest || {};
        if (f.engine === 'openvpn') {
            f.address = f.role === 'server' ? (sg.ov_site_address || '10.98.1.1/29') : '';
            f.listen_port = f.role === 'server' ? String(sg.ov_site_port || 1195) : '';
            if (f.role === 'server' && !f.remote) f.remote = data.wan_address ? `${data.wan_address}:${f.listen_port}` : '';
        } else {
            f.address = sg.site_address || '';
            f.listen_port = String(sg.site_port || 51820);
        }
        siteEd.errors = {}; siteEd.message = null;
    }

    function sitePayload(f, editing, extra) {
        const list = f.remote_networks.split(/[\s,]+/).filter(Boolean);
        const body = { name: f.name, internet: f.internet, isolated: f.isolated, enabled: f.enabled };
        if (f.engine === 'openvpn') {
            if (!editing) { body.engine = 'openvpn'; body.role = f.role; }
            if (list.length) body.remote_networks = list; else if (editing || f.role === 'server') body.remote_networks = [];
            if (f.role === 'server') {
                body.address = f.address; body.remote = f.remote; body.proto = f.proto;
                const port = parseInt(f.listen_port, 10);
                body.listen_port = Number.isNaN(port) ? 0 : port;
            } else if (f.file.trim() !== '') body.file = f.file;
            return Object.assign(body, extra || {});
        }
        body.address = f.address; body.peer_public_key = f.peer_public_key; body.endpoint = f.endpoint; body.remote_networks = list;
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
            method: ed.id ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(sitePayload(f, !!ed.id, extra)) });
        busy = false;
        if (!res || !res.ok) {
            const sp = spread(await errorOf(res, 'Saving failed.'), SITE_FIELDS, SITE_RENAME);
            ed.errors = sp.errors; ed.message = sp.message;
            render();
            return;
        }
        const made = await res.json();
        const wasNew = !ed.id;
        siteEd = null; secretShown = null;
        fileShown = made.file ? { title: `File for the other router of ${made.name}`, filename: made.file.filename, config: made.file.config, kind: 'peer' } : null;
        await load();
        let note;
        if (made.file) note = (wasNew ? 'Added to the draft.' : 'Saved as a draft.') + ' Give the other router the file below (its private key is in it: shown only once), then review and commit from the bar at the top.';
        else if (wasNew) note = f.engine === 'openvpn' ? 'Added to the draft. Review and commit it from the bar at the top; the tunnel starts then.' : 'Added to the draft. Give the other router this router\'s public key (under "Settings for the other router"), then review and commit from the bar at the top.';
        else note = 'Saved as a draft. Review and commit it from the bar at the top when you are ready.';
        showMessage(note, 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
        const card = $('vpn-file');
        if (card) card.scrollIntoView({ block: 'nearest' });
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

    // ---------------------------------------------------------------- client VPN: the servers

    function remoteSectionHtml(engine) {
        const r = engine === 'openvpn' ? data.ov_remote : data.remote;
        const lock = !editable() || busy || !!anyEditor();
        const name = ENGINE_NAME[engine];
        const id = engine === 'openvpn' ? 'ov' : 'wg';
        if (!r) {
            const blurb = engine === 'openvpn'
                ? 'The same, with OpenVPN: for devices and apps that use it. The router makes a certificate for each device (from its own certificate authority) and gives you a file to import.'
                : 'Lets laptops and phones connect to this router from anywhere, and reach the networks here as if they were at home. The router is the server: you add each device below and give it a small file to import into the WireGuard app.';
            return `<section class="card">
                <div class="net-card-head"><h2>Remote access with ${name}</h2><span class="tag">Not set up</span>
                    <button type="button" class="cfg-btn" id="vpn-${id}-setup" style="margin-left:auto"${lock ? ' disabled' : ''}>Set up</button></div>
                <p class="form-hint">${blurb} ${data.wan_address ? '' : 'The internet connection has no address right now.'}</p>
            </section>`;
        }
        const routes = (r.client_routes || []).map(code).join(', ');
        const connect = r.endpoint ? code(r.endpoint.includes(':') && !r.endpoint.endsWith(']') ? r.endpoint : r.endpoint + ':' + r.listen_port) + (engine === 'openvpn' ? ` <span class="form-hint">(${escapeHtml((r.proto || '').toUpperCase())})</span>` : '')
            : '<span class="net-warn">Not set: devices need an address to connect to. Edit and fill it in.</span>';
        return `<section class="card">
            <div class="net-card-head"><h2>Remote access with ${name}</h2>${r.pending ? '<span class="tag tag-draft">Draft</span>' : (r.enabled ? '<span class="tag tag-ok">On</span>' : '<span class="tag">Switched off</span>')}
                <span style="margin-left:auto;display:flex;gap:0.5rem">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-${id}-edit"${lock ? ' disabled' : ''}>Edit</button>
                <button type="button" class="cfg-btn cfg-btn--danger" id="vpn-${id}-delete"${lock ? ' disabled' : ''}>${confirmDelete === 'remote:' + engine ? 'Click again' : 'Remove'}</button></span></div>
            <p class="form-hint">${escapeHtml(r.text)}</p>
            <dl class="vpn-dl">
                <dt>Devices connect to</dt><dd>${connect}</dd>
                <dt>Devices send to the router</dt><dd>${r.full_tunnel ? 'All their traffic' : (routes || 'Nothing')}${r.dns ? '; names are looked up by this router' : ''}</dd>
                ${engine === 'wireguard' ? `<dt>The router's public key</dt><dd>${code(r.public_key)}</dd>` : ''}
            </dl>
        </section>`;
    }

    const REMOTE_FIELDS = ['endpoint', 'listen_port', 'network', 'routes', 'proto'];

    function remoteEditorHtml() {
        const ed = remoteEd, f = ed.form, lock = !editable() || busy;
        const engine = ed.engine, isOV = engine === 'openvpn';
        const existing = isOV ? data.ov_remote : data.remote;
        const wan = data.wan_address ? ` The internet connection's address right now is ${code(data.wan_address)}.` : '';
        return `<section class="card fw-editor" id="vpn-remote-editor">
            <div class="net-card-head"><h2>${existing ? 'Edit' : 'Set up'} remote access with ${ENGINE_NAME[engine]}</h2></div>
            <div class="form-grid">
                ${field(ed, 'Address devices connect to', 'vpn-r-endpoint', input(ed, 'vpn-r-endpoint', 'endpoint', f.endpoint, { placeholder: 'vpn.example.com' }), 'endpoint',
                    'A name (see <a href="#" data-goto="ddns">Dynamic DNS</a>) or an address of this router that can be reached from the internet. The port is added for you.' + wan, 'form-field--wide')}
                ${field(ed, 'Port', 'vpn-r-port', input(ed, 'vpn-r-port', 'listen_port', f.listen_port, { mode: 'numeric' }), 'listen_port', (isOV && f.proto === 'tcp' ? 'TCP' : 'UDP') + '. If this router is behind another router, forward this port to it.')}
                ${isOV ? field(ed, 'Protocol', 'vpn-r-proto', select(ed, 'vpn-r-proto', 'proto', f.proto, PROTOS), 'proto') : ''}
                ${field(ed, 'Network for the devices', 'vpn-r-net', input(ed, 'vpn-r-net', 'network', f.network, { placeholder: isOV ? '10.9.0.0/24' : '10.8.0.0/24' }), 'network', 'Devices get an address from here; the router takes the first one. It must not overlap any other network.')}
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
                ${!isOV && existing ? `<button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-r-newkey"${lock ? ' disabled' : ''} title="Makes a new key for the server; every device then needs a new file">New server key</button>` : ''}
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-r-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="vpn-r-save"${lock ? ' disabled' : ''}>${existing ? 'Save to draft' : 'Set up'}</button>
            </div></section>`;
    }

    function openRemoteEditor(engine) {
        const isOV = engine === 'openvpn';
        const r = isOV ? data.ov_remote : data.remote, sg = data.suggest || {};
        const form = r
            ? { endpoint: r.endpoint, listen_port: String(r.listen_port), proto: r.proto || 'udp', network: r.network, routes: (r.routes || []).join(', '), full_tunnel: r.full_tunnel, dns: r.dns, internet: r.internet, isolated: r.isolated, enabled: r.enabled }
            : { endpoint: data.wan_address || '', listen_port: String(isOV ? (sg.ov_remote_port || 1194) : (sg.remote_port || 51821)), proto: 'udp', network: isOV ? (sg.ov_remote_network || '10.9.0.0/24') : (sg.remote_network || '10.8.0.0/24'),
                routes: '', full_tunnel: false, dns: true, internet: false, isolated: false, enabled: true };
        remoteEd = { engine, form, errors: {}, message: null };
        render();
        const first = $('vpn-r-endpoint');
        if (first) first.focus();
    }

    async function saveRemote(extra) {
        const ed = remoteEd, f = ed.form, isOV = ed.engine === 'openvpn';
        ed.errors = {}; ed.message = null;
        const port = parseInt(f.listen_port, 10);
        const body = Object.assign({ endpoint: f.endpoint, listen_port: Number.isNaN(port) ? 0 : port, network: f.network, routes: f.routes.split(/[\s,]+/).filter(Boolean),
            full_tunnel: f.full_tunnel, dns: f.dns, internet: f.internet, isolated: f.isolated, enabled: f.enabled }, extra || {});
        if (isOV) body.proto = f.proto;
        busy = true; render();
        const res = await apiFetch(isOV ? '/api/v1/vpn/ov-remote' : '/api/v1/vpn/remote', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        busy = false;
        if (!res || !res.ok) {
            const sp = spread(await errorOf(res, 'Saving failed.'), REMOTE_FIELDS);
            ed.errors = sp.errors; ed.message = sp.message;
            render();
            return;
        }
        const was = !!(isOV ? data.ov_remote : data.remote);
        remoteEd = null;
        await load();
        showMessage(extra && extra.new_key ? 'A new server key is in the draft. Every device needs a new file once it is committed.' : (was ? 'Saved as a draft. Review and commit it from the bar at the top when you are ready.' : 'Remote access is in the draft. Add the devices, then review and commit from the bar at the top.'), 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    // ---------------------------------------------------------------- client VPN: the devices

    function deviceRowHtml(c, engine) {
        const lock = !editable() || busy || !!anyEditor();
        const id = escapeHtml(c.id);
        const warn = engine === 'openvpn' && c.expiring ? ` <span class="tag tag-draft" title="Make a new file for this device before then">Certificate runs out ${c.not_after ? escapeHtml(dateOf(c.not_after)) : ''}</span>` : '';
        return `<div class="fw-row fw-rule${c.enabled ? '' : ' fw-rule--off'} vpn-row">
            <label class="toggle fw-switch"><input type="checkbox" class="vpn-enable" data-kind="${engine}-client" data-id="${id}"${c.enabled ? ' checked' : ''}${lock ? ' disabled' : ''} aria-label="Switch on or off"></label>
            <div class="fw-main"><div class="fw-name"><strong>${escapeHtml(c.name)}</strong>${c.pending ? ' <span class="tag tag-draft">Draft</span>' : ''}${warn}</div>
                <div class="form-hint fw-text">${escapeHtml(c.text)}</div>
                <div class="vpn-status">${statusHtml(c, 'client', engine)}</div></div>
            <div class="fw-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost vpn-dev-rename" data-engine="${engine}" data-id="${id}"${lock ? ' disabled' : ''}>Rename</button>
                <button type="button" class="cfg-btn cfg-btn--ghost vpn-dev-newkey" data-engine="${engine}" data-id="${id}"${lock ? ' disabled' : ''} title="Makes new keys and a new file for this device; the old file stops working once committed">New file</button>
                <button type="button" class="cfg-btn cfg-btn--danger vpn-dev-delete" data-engine="${engine}" data-id="${id}"${lock ? ' disabled' : ''}>${confirmDelete === engine + 'client:' + c.id ? 'Click again' : 'Remove'}</button>
            </div></div>`;
    }

    function devicesCardHtml(engine) {
        const r = engine === 'openvpn' ? data.ov_remote : data.remote;
        if (!r) return '';
        const lock = !editable() || busy || !!anyEditor();
        const list = r.clients || [];
        const id = engine === 'openvpn' ? 'ov' : 'wg';
        return `<section class="card">
            <div class="net-card-head"><h2>Devices (${ENGINE_NAME[engine]})</h2><span class="tag">${list.length}</span>
                <button type="button" class="cfg-btn vpn-dev-add" data-engine="${engine}" id="vpn-${id}-dev-add" style="margin-left:auto"${lock || list.length >= 100 ? ' disabled' : ''}>Add a device</button></div>
            <p class="form-hint">Each laptop or phone gets its own keys and address, so one can be removed without touching the others. After adding a device you get a file for the ${engine === 'openvpn' ? 'OpenVPN app' : 'WireGuard app'} on it (the private key is in that file, shown only once).</p>
            <div class="fw-list">${list.length ? list.map(c => deviceRowHtml(c, engine)).join('') : '<div class="fw-row fw-empty">No devices yet.</div>'}</div>
        </section>`;
    }

    function deviceEditorHtml() {
        const ed = deviceEd, f = ed.form, lock = !editable() || busy;
        const renaming = !!ed.id, isOV = ed.engine === 'openvpn';
        return `<section class="card fw-editor" id="vpn-dev-editor">
            <div class="net-card-head"><h2>${renaming ? 'Rename device' : 'New device'} (${ENGINE_NAME[ed.engine]})</h2></div>
            <div class="form-grid">
                ${field(ed, 'Name', 'vpn-d-name', input(ed, 'vpn-d-name', 'name', f.name, { placeholder: 'Alec phone' }), 'name', 'Who or what it is; letters, digits, spaces and . _ ( ) + -')}
                ${renaming || isOV ? '' : field(ed, 'Public key of the device <span class="form-hint">(optional)</span>', 'vpn-d-key', input(ed, 'vpn-d-key', 'public_key', f.public_key, { placeholder: 'leave empty to let the router make the keys' }), 'public_key',
                    'Only if the device already made its own keys. Then the router never knows its private key, and you put that into the file yourself.', 'form-field--wide')}
            </div>
            ${renaming || isOV ? '' : `<div class="net-access">${check('preshared', 'Add an extra shared secret', 'A pre-shared key in the file adds protection for the future. The file has it.', f.preshared)}</div>`}
            ${ed.message ? `<div class="form-message" role="alert">${escapeHtml(ed.message)}</div>` : ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-d-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="vpn-d-save"${lock ? ' disabled' : ''}>${renaming ? 'Save to draft' : 'Add device'}</button>
            </div></section>`;
    }

    function fileCardHtml() {
        const f = fileShown;
        let intro;
        if (f.kind === 'peer') {
            intro = `<strong>Keep this file safe.</strong> It holds the private key of the other router, which this router does not keep: this is the only time it can be shown. On the other router: <em>VPN, Site to site, Add a tunnel, OpenVPN, This router connects</em>, and give it this file. (Another OpenVPN client can use it too; it then has to add the routes itself.) The tunnel works once the change is committed on this router.`;
        } else if (f.own) {
            intro = 'The device made its own keys, so this router does not know its private key. Replace the marked line with it, then import the file into the WireGuard app on the device. The device can connect once the change is committed.';
        } else if (f.kind === 'ov-device') {
            intro = `<strong>Keep this file safe.</strong> It holds the device's private key, which this router does not keep: this is the only time it can be shown. Import it into the OpenVPN app on the device (<em>OpenVPN Connect: Import profile, Upload file</em>; on Linux <code>openvpn --config</code>), then delete it. The device can connect once the change is committed.`;
        } else {
            intro = `<strong>Keep this file safe.</strong> It holds the device's private key, which this router does not keep: this is the only time it can be shown. Import it into the WireGuard app on the device (<em>Add tunnel, Import from file</em>; on a phone, send the file to it, a QR code is not built yet), then delete it. The device can connect once the change is committed.`;
        }
        return `<section class="card vpn-file" id="vpn-file">
            <div class="net-card-head"><h2>${escapeHtml(f.title)}</h2></div>
            <p class="form-hint">${intro}</p>
            <pre class="vpn-conf">${escapeHtml(f.config)}</pre>
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-file-copy">Copy</button>
                <button type="button" class="cfg-btn cfg-btn--ghost" id="vpn-file-download">Download ${escapeHtml(f.filename)}</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="vpn-file-done">I have it, hide this</button>
            </div></section>`;
    }

    function openDeviceEditor(engine, id) {
        const list = (engine === 'openvpn' ? data.ov_remote : data.remote).clients;
        const c = id ? list.find(x => x.id === id) : null;
        deviceEd = { engine, id: id || null, form: { name: c ? c.name : '', public_key: '', preshared: false }, errors: {}, message: null };
        render();
        const first = $('vpn-d-name');
        if (first) first.focus();
    }

    const DEVICE_FIELDS = ['name', 'public_key', 'endpoint'];
    const clientBase = (engine) => engine === 'openvpn' ? '/api/v1/vpn/ov-remote/clients' : '/api/v1/vpn/remote/clients';

    async function saveDevice() {
        const ed = deviceEd, f = ed.form, isOV = ed.engine === 'openvpn';
        ed.errors = {}; ed.message = null;
        const body = { name: f.name };
        if (!ed.id && !isOV) {
            if (f.public_key.trim() !== '') body.public_key = f.public_key.trim();
            if (f.preshared) body.preshared = true;
        }
        busy = true; render();
        const base = clientBase(ed.engine);
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
        fileShown = made.file ? { title: `File for ${made.client.name}`, filename: made.file.filename, config: made.file.config, kind: isOV ? 'ov-device' : 'wg-device' } : null;
        if (!made.file && wasNew && !isOV) {
            // the device has its own keys: the file is the same except for the place where its private key goes
            const t = await apiFetch(`${clientBase('wireguard')}/${encodeURIComponent(made.client.id)}/file`);
            if (t && t.ok) { const tf = await t.json(); fileShown = { title: `File for ${made.client.name}`, filename: tf.filename, config: tf.config, own: true, kind: 'own' }; }
        }
        await load();
        let note = wasNew ? 'Added to the draft. Review and commit it from the bar at the top; the device can connect then.' : 'Saved as a draft. Review and commit it from the bar at the top when you are ready.';
        if (wasNew && fileShown && fileShown.own) note += ' The device has its own keys: the file below has a place for its private key.';
        showMessage(note, 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
        const card = $('vpn-file');
        if (card) card.scrollIntoView({ block: 'nearest' });
    }

    async function newDeviceFile(engine, id) {
        busy = true; render();
        const body = engine === 'openvpn' ? { new_file: true } : { new_key: true };
        const res = await apiFetch(`${clientBase(engine)}/${encodeURIComponent(id)}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        busy = false;
        if (!res || !res.ok) { showMessage((await errorOf(res, 'It could not be done.')).general, 'error'); await load(); return; }
        const made = await res.json();
        fileShown = made.file ? { title: `File for ${made.client.name}`, filename: made.file.filename, config: made.file.config, kind: engine === 'openvpn' ? 'ov-device' : 'wg-device' } : null;
        await load();
        showMessage('New keys are in the draft. The old file stops working once you commit; import the new one below on the device.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    // ---------------------------------------------------------------- the authority

    function caCardHtml() {
        const ca = data.ca;
        if (!ca) return '';
        const soon = (ms) => ms && ms < Date.now() + 60 * 864e5;
        return `<section class="card">
            <div class="net-card-head"><h2>Certificates (OpenVPN)</h2></div>
            <p class="form-hint">OpenVPN needs certificates. This router is its own certificate authority: it makes and signs the certificates of the server and of each device, and keeps a list of the ones that are no longer accepted (a device that is removed, or that gets a new file). Nothing has to be made by hand.</p>
            <dl class="vpn-dl">
                <dt>Authority</dt><dd>${escapeHtml(ca.subject)}, valid until ${escapeHtml(dateOf(ca.not_after))}</dd>
                <dt>Server certificate</dt><dd>valid until ${escapeHtml(dateOf(ca.server_not_after))}${soon(ca.server_not_after) ? ' <span class="net-warn">Runs out soon.</span>' : ''}</dd>
                <dt>No longer accepted</dt><dd>${ca.revoked} certificate${ca.revoked === 1 ? '' : 's'}</dd>
            </dl>
        </section>`;
    }

    // ---------------------------------------------------------------- loading and the small actions

    function render() {
        if (!data) return;
        let body;
        if (tab === 'site') {
            body = siteListHtml() + (siteEd ? siteEditorHtml() : '') + (fileShown && fileShown.kind === 'peer' ? fileCardHtml() : '');
        } else {
            body = remoteSectionHtml('wireguard') + (remoteEd && remoteEd.engine === 'wireguard' ? remoteEditorHtml() : '') + devicesCardHtml('wireguard') + (deviceEd && deviceEd.engine === 'wireguard' ? deviceEditorHtml() : '') +
                remoteSectionHtml('openvpn') + (remoteEd && remoteEd.engine === 'openvpn' ? remoteEditorHtml() : '') + devicesCardHtml('openvpn') + (deviceEd && deviceEd.engine === 'openvpn' ? deviceEditorHtml() : '') +
                (fileShown && fileShown.kind !== 'peer' ? fileCardHtml() : '') + caCardHtml();
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

    const GONE = 'Removed from the draft. Review and commit it from the bar at the top when you are ready.';

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
            const id = btn.dataset.id, engine = btn.dataset.engine;
            switch (true) {
                case btn.id === 'vpn-site-add': openSiteEditor(null); break;
                case btn.id === 'vpn-s-save': saveSite(); break;
                case btn.id === 'vpn-s-newkey': saveSite({ new_key: true }); break;
                case btn.id === 'vpn-s-newfile': saveSite({ new_file: true }); break;
                case btn.id === 'vpn-s-cancel': siteEd = null; secretShown = null; render(); break;
                case btn.id === 'vpn-s-psk-make': makePresharedKey(); break;
                case btn.classList.contains('vpn-site-edit'): openSiteEditor(id); break;
                case btn.classList.contains('vpn-site-delete'): remove('site:' + id, `/api/v1/vpn/sites/${encodeURIComponent(id)}`, GONE); break;
                case btn.id === 'vpn-wg-setup' || btn.id === 'vpn-wg-edit': openRemoteEditor('wireguard'); break;
                case btn.id === 'vpn-ov-setup' || btn.id === 'vpn-ov-edit': openRemoteEditor('openvpn'); break;
                case btn.id === 'vpn-wg-delete': remove('remote:wireguard', '/api/v1/vpn/remote', 'Remote access and its devices are removed from the draft. Review and commit it from the bar at the top when you are ready.'); break;
                case btn.id === 'vpn-ov-delete': remove('remote:openvpn', '/api/v1/vpn/ov-remote', 'Remote access and its devices are removed from the draft (their certificates will no longer be accepted). Review and commit it from the bar at the top when you are ready.'); break;
                case btn.id === 'vpn-r-save': saveRemote(); break;
                case btn.id === 'vpn-r-newkey': saveRemote({ new_key: true }); break;
                case btn.id === 'vpn-r-cancel': remoteEd = null; render(); break;
                case btn.classList.contains('vpn-dev-add'): openDeviceEditor(engine, null); break;
                case btn.id === 'vpn-d-save': saveDevice(); break;
                case btn.id === 'vpn-d-cancel': deviceEd = null; render(); break;
                case btn.classList.contains('vpn-dev-rename'): openDeviceEditor(engine, id); break;
                case btn.classList.contains('vpn-dev-newkey'): newDeviceFile(engine, id); break;
                case btn.classList.contains('vpn-dev-delete'): remove(engine + 'client:' + id, `${clientBase(engine)}/${encodeURIComponent(id)}`, GONE); break;
                case btn.id === 'vpn-file-copy': copyFile(); break;
                case btn.id === 'vpn-file-download': downloadFile(); break;
                case btn.id === 'vpn-file-done': fileShown = null; render(); break;
            }
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.classList.contains('vpn-enable')) {
                const kind = el.dataset.kind, id = encodeURIComponent(el.dataset.id);
                const url = kind === 'site' ? `/api/v1/vpn/sites/${id}` : `${clientBase(kind.replace('-client', ''))}/${id}`;
                simple(url, 'PUT', { enabled: el.checked }, el.checked ? 'Switched on in the draft.' : 'Switched off in the draft.');
                return;
            }
            if (el.id === 'vpn-s-file-pick' && siteEd && el.files && el.files[0]) {
                const reader = new FileReader();
                reader.onload = () => { siteEd.form.file = String(reader.result || ''); delete siteEd.errors.file; render(); };
                reader.readAsText(el.files[0]);
                return;
            }
            const ed = anyEditor();
            if (!ed || !el.dataset.ed) return;
            if (el.type === 'checkbox') ed.form[el.dataset.ed] = el.checked;
            else if (el.type === 'radio' && siteEd) {
                siteEd.form[el.dataset.ed] = el.value;
                if (el.dataset.ed === 'engine' && el.value === 'openvpn' && !siteEd.form.role) siteEd.form.role = 'server';
                kindChanged();
                render();
            } else if (el.tagName === 'SELECT') {
                ed.form[el.dataset.ed] = el.value;
                if (remoteEd && el.dataset.ed === 'proto') render();
            }
        });
        body.addEventListener('input', (e) => {
            const el = e.target;
            const ed = anyEditor();
            if (!ed || !el.dataset.ed || el.type === 'checkbox' || el.type === 'radio' || el.tagName === 'SELECT') return;
            ed.form[el.dataset.ed] = el.value;
            delete ed.errors[el.dataset.ed];
            el.removeAttribute('aria-invalid');
            el.closest('.form-field')?.querySelector('.field-error')?.remove();
        });
        body.addEventListener('keydown', (e) => {
            const ed = anyEditor();
            if (e.key === 'Enter' && ed && e.target.dataset.ed && e.target.tagName === 'INPUT' && e.target.type !== 'checkbox' && e.target.type !== 'radio' && !busy) {
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
            if (anyEditor() || busy) return;
            const res = await apiFetch('/api/v1/vpn');
            if (res && res.ok && !anyEditor() && !busy) { data = await res.json(); render(); }
        }, 10000);
        return () => { clearInterval(timer); timer = null; fileShown = null; secretShown = null; };
    }

    return { init };
})();
