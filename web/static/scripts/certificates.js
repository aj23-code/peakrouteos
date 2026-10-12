// web/static/scripts/certificates.js
// Certificates page: the certificate of the web interface (built-in, made by the router's own web authority, or one you upload), how to
// make a computer or phone trust the authority, and the OpenVPN certificates. Everything here applies at once, except renewing the
// OpenVPN server's certificate, which goes through the draft because the VPN servers restart. docs/certificates.md has the rules.

window.CertificatesModule = (function () {
    let web = null;              // GET /api/v1/certificates
    let vpn = null;              // GET /api/v1/vpn (for the VPN card and for suggestions)
    let ddns = null;             // GET /api/v1/ddns (names to suggest)
    let names = '';              // the text of the extra names field
    let confirmReplace = false;
    let busy = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const code = (t) => `<code class="vpn-key">${escapeHtml(t)}</code>`;
    const dateOf = (ms) => new Date(ms).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });

    function showMessage(text, kind) {
        const box = $('certs-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    async function errorText(res, fallback) {
        if (!res) return 'The router did not answer. Try again.';
        try { const e = await res.json(); return e.error || fallback; } catch (e) { return fallback; }
    }

    function expiryHtml(d) {
        if (!d) return '';
        const soon = d.days_left < 30;
        return `${escapeHtml(dateOf(d.not_after))} <span class="${soon ? 'net-warn' : 'form-hint'}">(${d.days_left < 0 ? 'ran out' : d.days_left + ' days left'})</span>`;
    }

    // ---------------------------------------------------------------- the web interface

    function suggestions() {
        const have = new Set((web.wanted || []).concat(web.extra_names || []).map(s => s.toLowerCase()));
        const out = [];
        const add = (s) => { s = (s || '').toLowerCase().replace(/:\d+$/, ''); if (s && !have.has(s) && !out.includes(s)) out.push(s); };
        if (ddns && ddns.entries) ddns.entries.forEach(e => add(e.hostname));
        if (vpn) {
            if (vpn.remote && vpn.remote.endpoint) add(vpn.remote.endpoint);
            if (vpn.ov_remote && vpn.ov_remote.endpoint) add(vpn.ov_remote.endpoint);
        }
        if (vpn && vpn.wan_address) add(vpn.wan_address);
        return out;
    }

    function webCardHtml() {
        const w = web.web;
        const custom = web.mode === 'custom';
        if (!w) return '<section class="card"><div class="net-card-head"><h2>Web interface</h2></div><p class="form-hint net-warn">The certificate cannot be read.</p></section>';
        const missing = (web.missing || []).length
            ? `<p class="form-hint net-warn">${custom ? 'This certificate is not good for' : 'The certificate is not yet good for'} ${web.missing.map(code).join(', ')}, which ${custom ? 'this router has: a browser would complain when you use them.' : 'this router has. It is made again within the day, or press <em>Make a new one</em>.'}</p>` : '';
        const sug = suggestions();
        return `<section class="card">
            <div class="net-card-head"><h2>Web interface</h2>${custom ? '<span class="tag">Custom</span>' : '<span class="tag tag-ok">Built-in</span>'}${w.days_left < 30 ? '<span class="tag tag-draft">Runs out soon</span>' : ''}</div>
            <p class="form-hint">${custom
                ? 'A certificate you uploaded is in use. It is never replaced or renewed by the router: put in a new one before it runs out.'
                : 'Made by this router\'s own web authority, for its names and addresses, and renewed by itself a month before it runs out. A browser warns until it trusts the authority (below).'}</p>
            <dl class="vpn-dl">
                <dt>Good for</dt><dd>${(w.names || []).map(code).join(', ') || 'nothing'}</dd>
                <dt>Made by</dt><dd>${escapeHtml(w.issuer)}${web.self_signed ? ' <span class="form-hint">(it vouches for itself)</span>' : ''}</dd>
                <dt>Valid until</dt><dd>${expiryHtml(w)}</dd>
                <dt>Fingerprint</dt><dd>${code(w.fingerprint)}</dd>
            </dl>
            ${missing}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="certs-regen"${busy ? ' disabled' : ''}>${custom ? (confirmReplace ? 'Click again to replace it' : 'Use the built-in certificate instead') : 'Make a new one'}</button>
            </div>
            <h3 class="form-section">More names for the built-in certificate</h3>
            <p class="form-hint">The router's own names and the addresses of its networks are in it already. Add a name or address you also use to reach the router, such as the name from <a href="#" data-goto="ddns">Dynamic DNS</a>, so that a browser does not complain there. Separate with commas.</p>
            <div class="form-grid"><div class="form-field form-field--wide"><label for="certs-names">Names and addresses</label>
                <input type="text" id="certs-names" value="${escapeHtml(names)}" placeholder="router.example.com, 203.0.113.9" autocomplete="off" spellcheck="false" autocapitalize="off"${busy ? ' disabled' : ''}></div></div>
            ${sug.length ? `<p class="form-hint">Suggested: ${sug.map(s => `<button type="button" class="cfg-btn cfg-btn--ghost certs-sug" data-name="${escapeHtml(s)}">${escapeHtml(s)}</button>`).join(' ')}</p>` : ''}
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--primary" id="certs-names-save"${busy ? ' disabled' : ''}>Save${custom ? '' : ' and make a new certificate'}</button></div>
            ${custom ? '<p class="form-hint">These names are used when the built-in certificate is in use again.</p>' : ''}
        </section>
        <section class="card">
            <div class="net-card-head"><h2>Use your own certificate</h2></div>
            <p class="form-hint">Upload a certificate and its private key (PEM files), for example one from a public authority for a name that points at this router. If the certificate file holds a chain, the router's certificate must come first. It is checked (it must be valid and the key must belong to it) before anything is replaced, and it is served at once.</p>
            <div class="form-grid">
                <div class="form-field"><label for="certs-file-cert">Certificate (.crt, .pem)</label><input type="file" id="certs-file-cert" accept=".crt,.pem,.cer,text/plain"${busy ? ' disabled' : ''}></div>
                <div class="form-field"><label for="certs-file-key">Private key (.key, .pem)</label><input type="file" id="certs-file-key" accept=".key,.pem,text/plain"${busy ? ' disabled' : ''}></div>
            </div>
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--primary" id="certs-upload"${busy ? ' disabled' : ''}>Install</button></div>
        </section>`;
    }

    function authorityCardHtml() {
        const a = web.authority;
        return `<section class="card">
            <div class="net-card-head"><h2>Trusting this router</h2></div>
            <p class="form-hint">The router's built-in certificate is made by its own web authority. Browsers do not know it, so they warn. Install the authority's certificate once on each computer or phone you use, and the warnings stop. It is a public file: it lets a device recognise this router; the key that makes certificates never leaves the router. It is separate from the VPN's authority, so nothing made for a VPN device is trusted for the web interface. Only install it on devices of your own.</p>
            ${a ? `<dl class="vpn-dl">
                <dt>Authority</dt><dd>${escapeHtml(a.subject)}</dd>
                <dt>Valid until</dt><dd>${expiryHtml(a)}</dd>
                <dt>Fingerprint</dt><dd>${code(a.fingerprint)}</dd>
            </dl>` : '<p class="form-hint">It is made when you download it.</p>'}
            <div class="form-actions"><a class="cfg-btn cfg-btn--primary" href="/api/v1/certificates/authority.crt" download="peakroute-web-ca.crt">Download the authority's certificate</a></div>
            <details class="vpn-details"><summary>How to install it</summary>
                <dl class="vpn-dl">
                    <dt>Windows</dt><dd>Open the file, <em>Install Certificate</em>, <em>Local Machine</em> (or <em>Current User</em>), put it in <em>Trusted Root Certification Authorities</em>.</dd>
                    <dt>macOS</dt><dd>Open the file (Keychain Access), add it to <em>System</em>, open it, and under <em>Trust</em> choose <em>Always Trust</em> for <em>Secure Sockets Layer</em>.</dd>
                    <dt>iPhone, iPad</dt><dd>Open the file (from Mail or Files), <em>Allow</em>, then Settings, General, VPN &amp; Device Management, install the profile. Then Settings, General, About, Certificate Trust Settings, and switch it on.</dd>
                    <dt>Android</dt><dd>Settings, Security, Encryption &amp; credentials, Install a certificate, <em>CA certificate</em>, and pick the file.</dd>
                    <dt>Linux, Chrome</dt><dd>Settings, Privacy and security, Security, Manage certificates, <em>Authorities</em>, Import.</dd>
                    <dt>Firefox</dt><dd>Settings, Privacy &amp; Security, Certificates, View Certificates, <em>Authorities</em>, Import, and tick <em>Trust this CA to identify websites</em>.</dd>
                </dl>
                <p class="form-hint">Compare the fingerprint above with the one the device shows before you trust it.</p>
            </details>
        </section>`;
    }

    // ---------------------------------------------------------------- the VPN

    function vpnCardHtml() {
        if (!vpn) return '';
        const ca = vpn.ca;
        if (!ca) {
            return `<section class="card">
                <div class="net-card-head"><h2>VPN (OpenVPN)</h2><span class="tag">Not used</span></div>
                <p class="form-hint">OpenVPN needs certificates. This router makes its own certificate authority the first time you set up an OpenVPN server or a tunnel that waits (<a href="#" data-goto="vpn">VPN</a>). WireGuard uses keys, not certificates.</p>
            </section>`;
        }
        const soon = (ms) => ms && ms < Date.now() + 60 * 864e5;
        const rows = [];
        if (vpn.ov_remote) (vpn.ov_remote.clients || []).forEach(c => rows.push({ name: c.name, kind: 'Remote access device', ms: c.not_after }));
        (vpn.sites || []).forEach(s => { if (s.engine === 'openvpn' && s.role === 'client' && s.cert_expires) rows.push({ name: s.name, kind: 'Site tunnel (this router connects)', ms: s.cert_expires }); });
        return `<section class="card">
            <div class="net-card-head"><h2>VPN (OpenVPN)</h2><span class="tag tag-ok">In use</span></div>
            <p class="form-hint">This router is the certificate authority of its OpenVPN servers: it makes and signs the certificates of the servers and of each device, and keeps a list of the ones that are no longer accepted. This authority is separate from the web interface's. Devices are managed on <a href="#" data-goto="vpn">VPN</a>.</p>
            <dl class="vpn-dl">
                <dt>Authority</dt><dd>${escapeHtml(ca.subject)}, valid until ${escapeHtml(dateOf(ca.not_after))}</dd>
                <dt>Server certificate</dt><dd>valid until ${escapeHtml(dateOf(ca.server_not_after))}${soon(ca.server_not_after) ? ' <span class="net-warn">Runs out soon: renew it.</span>' : ''}</dd>
                <dt>No longer accepted</dt><dd>${ca.revoked} certificate${ca.revoked === 1 ? '' : 's'}</dd>
            </dl>
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--ghost" id="certs-renew-server"${busy ? ' disabled' : ''} title="Makes the OpenVPN servers a new certificate. It goes into the draft; the servers restart when you commit, and devices reconnect by themselves.">Renew the server certificate</button></div>
            ${rows.length ? `<div class="table-container"><table class="data-table"><thead><tr><th>Name</th><th>What</th><th>Valid until</th></tr></thead><tbody>${rows.map(r =>
                `<tr><td>${escapeHtml(r.name)}</td><td>${escapeHtml(r.kind)}</td><td>${r.ms ? escapeHtml(dateOf(r.ms)) + (soon(r.ms) ? ' <span class="net-warn">soon</span>' : '') : ''}</td></tr>`).join('')}</tbody></table></div>` : ''}
        </section>`;
    }

    function render() {
        if (!web) return;
        $('certs-body').innerHTML = webCardHtml() + authorityCardHtml() + vpnCardHtml();
    }

    // ---------------------------------------------------------------- loading and saving

    async function load() {
        const [r1, r2, r3] = await Promise.all([apiFetch('/api/v1/certificates'), apiFetch('/api/v1/vpn'), apiFetch('/api/v1/ddns')]);
        if (!r1 || !r1.ok) { showMessage('Could not load the certificates.', 'error'); return; }
        web = await r1.json();
        vpn = r2 && r2.ok ? await r2.json() : null;
        ddns = r3 && r3.ok ? await r3.json() : null;
        names = (web.extra_names || []).join(', ');
        render();
    }

    async function regenerate() {
        const custom = web.mode === 'custom';
        if (custom && !confirmReplace) {
            confirmReplace = true; render();
            setTimeout(() => { if (confirmReplace) { confirmReplace = false; render(); } }, 5000);
            return;
        }
        confirmReplace = false;
        busy = true; render();
        const res = await apiFetch('/api/v1/certificates/regenerate', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ replace_custom: custom }) });
        busy = false;
        if (!res || !res.ok) { showMessage(await errorText(res, 'The certificate could not be made.'), 'error'); render(); return; }
        web = await res.json();
        names = (web.extra_names || []).join(', ');
        showMessage('A new certificate is in use. Browsers that already have this page open keep the old one until they reconnect.', 'ok');
        render();
    }

    async function saveNames() {
        const list = names.split(/[\s,]+/).filter(Boolean);
        busy = true; render();
        const res = await apiFetch('/api/v1/certificates/names', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ names: list }) });
        busy = false;
        if (!res || !res.ok) { showMessage(await errorText(res, 'The names could not be saved.'), 'error'); render(); return; }
        web = await res.json();
        names = (web.extra_names || []).join(', ');
        showMessage(web.mode === 'custom' ? 'Saved. They are used when the built-in certificate is in use.' : 'Saved, and the certificate is made again for them.', 'ok');
        render();
    }

    async function upload() {
        const c = $('certs-file-cert').files[0], k = $('certs-file-key').files[0];
        if (!c || !k) { showMessage('Choose the certificate file and the private key file.', 'error'); return; }
        const form = new FormData();
        form.append('cert', c);
        form.append('key', k);
        busy = true; render();
        const res = await apiFetch('/api/v1/certificates/upload', { method: 'POST', body: form });
        busy = false;
        if (!res || !res.ok) { showMessage(await errorText(res, 'The certificate could not be installed.'), 'error'); render(); return; }
        web = await res.json();
        showMessage('The certificate is installed and in use.', 'ok');
        render();
    }

    async function renewServer() {
        busy = true; render();
        const res = await apiFetch('/api/v1/vpn/ca/renew-server', { method: 'POST' });
        busy = false;
        if (!res || !res.ok) { showMessage(await errorText(res, 'The certificate could not be renewed.'), 'error'); render(); return; }
        vpn = await res.json();
        showMessage('A new server certificate is in the draft. Review and commit it from the bar at the top; the OpenVPN servers restart then and devices reconnect by themselves.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
        render();
    }

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('certs-body');
        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            if (btn.id === 'certs-regen') regenerate();
            else if (btn.id === 'certs-names-save') saveNames();
            else if (btn.id === 'certs-upload') upload();
            else if (btn.id === 'certs-renew-server') renewServer();
            else if (btn.classList.contains('certs-sug')) {
                names = (names.trim() ? names.trim().replace(/,\s*$/, '') + ', ' : '') + btn.dataset.name;
                render();
            }
        });
        body.addEventListener('input', (e) => { if (e.target.id === 'certs-names') names = e.target.value; });
        body.addEventListener('keydown', (e) => { if (e.key === 'Enter' && e.target.id === 'certs-names' && !busy) { e.preventDefault(); saveNames(); } });
    }

    function init() {
        wire();
        showMessage('');
        web = null; vpn = null; ddns = null; confirmReplace = false; busy = false;
        load();
        return () => {};
    }

    return { init };
})();
